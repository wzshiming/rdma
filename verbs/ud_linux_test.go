//go:build linux && !android && (amd64 || arm64 || loong64 || ppc64le || riscv64 || s390x)

package verbs_test

import (
	"errors"
	"net/netip"
	"os"
	"syscall"
	"testing"
	"time"

	"github.com/wzshiming/rdma/internal/rdmatest"
	"github.com/wzshiming/rdma/verbs"
)

// ownAddress returns the address vector of the device's own GID: datagrams
// sent there loop back to the device.
func ownAddress(t *testing.T, dev rdmatest.Dev, ctx *verbs.Context) verbs.AHAttr {
	t.Helper()
	if dev.GIDIndex < 0 {
		t.Skip("iWARP has no datagram service")
	}
	port := uint8(dev.Port)
	pa, err := ctx.QueryPort(port)
	if err != nil {
		t.Fatalf("QueryPort(%d) error: %v", port, err)
	}
	gids, err := ctx.GIDTable(port)
	if err != nil {
		t.Fatalf("GIDTable(%d) error: %v", port, err)
	}
	for _, g := range gids {
		if g.Index == dev.GIDIndex {
			return verbs.AHAttr{
				IsGlobal: true,
				DLID:     pa.LID,
				Port:     port,
				GRH:      verbs.GlobalRoute{DGID: g.GID, SGIDIndex: uint8(dev.GIDIndex), HopLimit: 64},
			}
		}
	}
	t.Fatalf("GIDTable(%d) has no entry at index %d", port, dev.GIDIndex)
	return verbs.AHAttr{}
}

func TestCreateAH(t *testing.T) {
	dev, ctx := openDevice(t)
	pd := allocPD(t, ctx)
	attr := ownAddress(t, dev, ctx)
	ah, err := pd.CreateAH(&attr)
	if err != nil {
		t.Fatalf("CreateAH(%+v) error: %v", attr, err)
	}
	if ah.Pointer() == nil {
		t.Error("Pointer() = nil")
	}
	// The wrapper's Close leaves the handle to its owner, whose Close then
	// destroys it.
	if err := verbs.AHFromPointer(ah.Pointer()).Close(); err != nil {
		t.Errorf("AHFromPointer(...).Close() error: %v", err)
	}
	if err := ah.Close(); err != nil {
		t.Fatalf("Close() error: %v", err)
	}
	if ah.Pointer() != nil {
		t.Errorf("Pointer() after Close() = %p, want nil", ah.Pointer())
	}
	if err := ah.Close(); err != nil {
		t.Errorf("second Close() error: %v", err)
	}
}

// A failing ibv_create_ah is reported as every failing call is.
func TestCreateAHOnNoPort(t *testing.T) {
	dev, ctx := openDevice(t)
	pd := allocPD(t, ctx)
	attr := ownAddress(t, dev, ctx)
	attr.Port = 0
	ah, err := pd.CreateAH(&attr)
	if err == nil {
		ah.Close()
		t.Fatal("CreateAH() on port 0 succeeded, want an error")
	}
	var opErr *verbs.OpError
	var errno syscall.Errno
	if !errors.As(err, &opErr) || opErr.Op != "ibv_create_ah" || !errors.As(err, &errno) {
		t.Errorf("CreateAH() on port 0 error = %v, want an *OpError of ibv_create_ah with an errno", err)
	}
	t.Logf("CreateAH() on port 0: %v", err)
}

// udQKey is the qkey of the queue pairs of udPair, the one rdma_cm gives the
// datagrams of its UDP port space.
const udQKey = 0x01234567

// grhLen is the room a UD receive buffer leaves for the global routing header.
const grhLen = 40

// udPair is two UD queue pairs on the rdmatest device, each completing on its
// own CQ, with a buffer each and an address handle for the device's own GID.
type udPair struct {
	ctx      *verbs.Context
	pd       *verbs.PD
	attr     verbs.AHAttr
	ah       *verbs.AH
	cqA, cqB *verbs.CQ
	a, b     *verbs.QP
	mrA, mrB *verbs.MR
}

func newUDPair(t *testing.T) *udPair {
	t.Helper()
	dev, ctx := openDevice(t)
	p := &udPair{ctx: ctx, pd: allocPD(t, ctx), attr: ownAddress(t, dev, ctx)}
	p.cqA = createCQ(t, ctx, nil)
	p.cqB = createCQ(t, ctx, nil)
	p.a = createUD(t, p.pd, p.cqA, p.attr.Port)
	p.b = createUD(t, p.pd, p.cqB, p.attr.Port)
	p.mrA = allocMR(t, p.pd, 4096)
	p.mrB = allocMR(t, p.pd, 4096)
	var err error
	if p.ah, err = p.pd.CreateAH(&p.attr); err != nil {
		t.Fatalf("CreateAH(%+v) error: %v", p.attr, err)
	}
	t.Cleanup(func() {
		if err := p.ah.Close(); err != nil {
			t.Errorf("AH.Close() error: %v", err)
		}
	})
	return p
}

// createUD creates a UD queue pair completing on cq and brings it to RTS
// through the states of a datagram queue pair: INIT with its port, pkey index
// and qkey, RTR, and RTS with its send PSN.
func createUD(t *testing.T, pd *verbs.PD, cq *verbs.CQ, port uint8) *verbs.QP {
	t.Helper()
	attr := verbs.QPInitAttr{
		SendCQ: cq,
		RecvCQ: cq,
		Type:   verbs.QPTypeUD,
		Cap:    verbs.QPCap{MaxSendWR: 16, MaxRecvWR: 16, MaxSendSGE: 1, MaxRecvSGE: 1},
	}
	qp, err := pd.CreateQP(&attr)
	if err != nil {
		t.Fatalf("CreateQP(UD) error: %v", err)
	}
	t.Cleanup(func() {
		if err := qp.Close(); err != nil {
			t.Errorf("QP.Close() error: %v", err)
		}
	})
	err = qp.Modify(&verbs.QPAttr{State: verbs.QPStateInit, PkeyIndex: 0, Port: port, QKey: udQKey}, verbs.QPAttrState|verbs.QPAttrPkeyIndex|verbs.QPAttrPort|verbs.QPAttrQKey)
	if err != nil {
		t.Fatalf("Modify(INIT) error: %v", err)
	}
	if err := qp.Modify(&verbs.QPAttr{State: verbs.QPStateRTR}, verbs.QPAttrState); err != nil {
		t.Fatalf("Modify(RTR) error: %v", err)
	}
	if err := qp.Modify(&verbs.QPAttr{State: verbs.QPStateRTS, SQPSN: 0}, verbs.QPAttrState|verbs.QPAttrSQPSN); err != nil {
		t.Fatalf("Modify(RTS) error: %v", err)
	}
	return qp
}

// postRecv posts a receive of b into the front of its buffer, with room for
// the header and 64 bytes of payload.
func (p *udPair) postRecv(t *testing.T, id uint64) {
	t.Helper()
	buf := p.mrB.Bytes()[:grhLen+64]
	clear(buf)
	if err := p.b.PostRecv(verbs.RecvWR{ID: id, SGE: []verbs.SGE{p.mrB.SGE(buf)}}); err != nil {
		t.Fatalf("PostRecv() error: %v", err)
	}
}

// postDatagram posts msg from qp, from the front of a's buffer, as a
// signaled datagram to b through ah with qkey.
func (p *udPair) postDatagram(t *testing.T, qp *verbs.QP, ah *verbs.AH, qkey uint32, id uint64, msg string) {
	t.Helper()
	buf := p.mrA.Bytes()[:len(msg)]
	copy(buf, msg)
	wr := verbs.SendWR{ID: id, Op: verbs.WRSend, SGE: []verbs.SGE{p.mrA.SGE(buf)}, Flags: verbs.SendSignaled, AH: ah, RemoteQPN: p.b.Num(), RemoteQKey: qkey}
	if err := qp.PostSend(wr); err != nil {
		t.Fatalf("PostSend() error: %v", err)
	}
}

// payload returns what the receive completion recv put after the header.
func (p *udPair) payload(recv verbs.WC) []byte {
	if recv.Len < grhLen {
		return nil
	}
	return p.mrB.Bytes()[grhLen:recv.Len]
}

// pollFor collects the completions cq yields within d.
func pollFor(t *testing.T, cq *verbs.CQ, d time.Duration) []verbs.WC {
	t.Helper()
	var got []verbs.WC
	buf := make([]verbs.WC, 4)
	for deadline := time.Now().Add(d); time.Now().Before(deadline); time.Sleep(time.Millisecond) {
		n, err := cq.Poll(buf)
		if err != nil {
			t.Fatalf("Poll() error: %v", err)
		}
		got = append(got, buf[:n]...)
	}
	return got
}

func TestUDLoopback(t *testing.T) {
	p := newUDPair(t)

	t.Run("Datagram", func(t *testing.T) {
		const msg = "datagram over soft-roce"
		p.postRecv(t, 1)
		p.postDatagram(t, p.a, p.ah, udQKey, 2, msg)
		recv := pollOne(t, p.cqB)
		t.Logf("receive completion: %+v", recv)
		t.Logf("header: % x", p.mrB.Bytes()[:grhLen])
		if recv.ID != 1 || recv.Opcode != verbs.WCOpRecv || recv.QPNum != p.b.Num() {
			t.Errorf("receive completion = %+v, want id 1, RECV on QP %d", recv, p.b.Num())
		}
		if int(recv.Len) != grhLen+len(msg) {
			t.Errorf("receive completion has %d bytes, want the %d of the header and the %d of the payload", recv.Len, grhLen, len(msg))
		}
		if recv.Flags&verbs.WCGRH == 0 {
			t.Errorf("receive completion flags = %v, want GRH", recv.Flags)
		}
		if recv.SrcQP != p.a.Num() {
			t.Errorf("receive completion names source QP %d, want the sender's %d", recv.SrcQP, p.a.Num())
		}
		if got := p.payload(recv); string(got) != msg {
			t.Errorf("payload after the header = %q, want %q", got, msg)
		}
		if send := pollOne(t, p.cqA); send.ID != 2 || send.Opcode != verbs.WCOpSend {
			t.Errorf("send completion = %+v, want id 2, SEND", send)
		}
	})

	t.Run("WrongQKey", func(t *testing.T) {
		before, err := p.ctx.QueryPort(p.attr.Port)
		if err != nil {
			t.Fatalf("QueryPort() error: %v", err)
		}
		p.postRecv(t, 3)
		p.postDatagram(t, p.a, p.ah, udQKey+1, 4, "wrong qkey")
		t.Logf("send completion of the datagram with the wrong qkey: %+v", pollOne(t, p.cqA))
		if wcs := pollFor(t, p.cqB, time.Second); len(wcs) != 0 {
			t.Fatalf("a datagram with qkey %#x reached a queue pair with qkey %#x: %+v", udQKey+1, udQKey, wcs)
		}
		after, err := p.ctx.QueryPort(p.attr.Port)
		if err != nil {
			t.Fatalf("QueryPort() error: %v", err)
		}
		t.Logf("Q_Key violation counter of port %d: %d before, %d after", p.attr.Port, before.QKeyViolCntr, after.QKeyViolCntr)

		// The receive is still there for the next datagram with the right qkey.
		p.postDatagram(t, p.a, p.ah, udQKey, 5, "right qkey")
		recv := pollOne(t, p.cqB)
		if recv.ID != 3 || string(p.payload(recv)) != "right qkey" {
			t.Errorf("receive completion = %+v with %q, want id 3 with %q", recv, p.payload(recv), "right qkey")
		}
		pollOne(t, p.cqA)
	})

	// Wrappers of handles made elsewhere send as their owners do, and the
	// address handle's wrapper closes without destroying it.
	t.Run("Wrappers", func(t *testing.T) {
		ah := verbs.AHFromPointer(p.ah.Pointer())
		p.postRecv(t, 6)
		p.postDatagram(t, verbs.QPFromPointer(p.a.Pointer()), ah, udQKey, 7, "through wrappers")
		if recv := pollOne(t, p.cqB); recv.ID != 6 || string(p.payload(recv)) != "through wrappers" {
			t.Errorf("receive completion = %+v with %q, want id 6 with %q", recv, p.payload(recv), "through wrappers")
		}
		pollOne(t, p.cqA)
		if err := ah.Close(); err != nil {
			t.Fatalf("AHFromPointer(...).Close() error: %v", err)
		}
		p.postRecv(t, 8)
		p.postDatagram(t, p.a, p.ah, udQKey, 9, "after the wrapper closed")
		if recv := pollOne(t, p.cqB); recv.ID != 8 || string(p.payload(recv)) != "after the wrapper closed" {
			t.Errorf("receive completion = %+v with %q, want id 8 with %q", recv, p.payload(recv), "after the wrapper closed")
		}
		pollOne(t, p.cqA)
	})

	// A datagram without an address handle, or to a closed one, is refused
	// before it reaches the provider, which would follow the pointer.
	t.Run("Misuse", func(t *testing.T) {
		datagram := verbs.SendWR{ID: 10, Op: verbs.WRSend, Flags: verbs.SendSignaled, RemoteQPN: p.b.Num(), RemoteQKey: udQKey}
		if err := p.a.PostSend(datagram); !errors.Is(err, syscall.EINVAL) {
			t.Errorf("PostSend() without an address handle error = %v, want EINVAL", err)
		}
		closed, err := p.pd.CreateAH(&p.attr)
		if err != nil {
			t.Fatalf("CreateAH() error: %v", err)
		}
		if err := closed.Close(); err != nil {
			t.Fatalf("Close() error: %v", err)
		}
		datagram.AH = closed
		if err := p.a.PostSend(datagram); !errors.Is(err, os.ErrClosed) {
			t.Errorf("PostSend() to a closed address handle error = %v, want os.ErrClosed", err)
		}
		if wcs := pollFor(t, p.cqA, 100*time.Millisecond); len(wcs) != 0 {
			t.Errorf("refused requests completed: %+v", wcs)
		}
	})
}

// The completion of a datagram and its header give the address handle that
// answers the sender, as udaddy's server answers its client.
func TestUDReplyThroughTheCompletion(t *testing.T) {
	p := newUDPair(t)
	const ask, answer = "ask", "answer"
	p.postRecv(t, 1)
	p.postDatagram(t, p.a, p.ah, udQKey, 2, ask)
	recv := pollOne(t, p.cqB)
	pollOne(t, p.cqA)
	ah, err := p.pd.CreateAHFromWC(&recv, p.mrB.Bytes()[:grhLen], p.attr.Port)
	if err != nil {
		t.Fatalf("CreateAHFromWC(%+v) error: %v", recv, err)
	}
	t.Cleanup(func() {
		if err := ah.Close(); err != nil {
			t.Errorf("AH.Close() error: %v", err)
		}
	})
	in := p.mrA.Bytes()[2048 : 2048+grhLen+64]
	if err := p.a.PostRecv(verbs.RecvWR{ID: 3, SGE: []verbs.SGE{p.mrA.SGE(in)}}); err != nil {
		t.Fatalf("PostRecv() error: %v", err)
	}
	out := p.mrB.Bytes()[2048 : 2048+len(answer)]
	copy(out, answer)
	wr := verbs.SendWR{ID: 4, Op: verbs.WRSend, SGE: []verbs.SGE{p.mrB.SGE(out)}, Flags: verbs.SendSignaled, AH: ah, RemoteQPN: recv.SrcQP, RemoteQKey: udQKey}
	if err := p.b.PostSend(wr); err != nil {
		t.Fatalf("PostSend() error: %v", err)
	}
	reply := pollOne(t, p.cqA)
	if reply.ID != 3 || reply.SrcQP != p.b.Num() || reply.Len < grhLen || string(in[grhLen:reply.Len]) != answer {
		t.Errorf("reply completion = %+v with %q, want id 3 from QP %d with %q", reply, in[grhLen:max(reply.Len, grhLen)], p.b.Num(), answer)
	}
	if send := pollOne(t, p.cqB); send.ID != 4 || send.Opcode != verbs.WCOpSend {
		t.Errorf("send completion = %+v, want id 4, SEND", send)
	}
}

// checkOpError fails t unless err is an *OpError of op with an errno.
func checkOpError(t *testing.T, err error, op string) {
	t.Helper()
	var opErr *verbs.OpError
	var errno syscall.Errno
	if !errors.As(err, &opErr) || opErr.Op != op || !errors.As(err, &errno) {
		t.Errorf("error = %v, want an *OpError of %s with an errno", err, op)
	}
}

// On RoCE v2 a multicast group is its IP address as a GID, and its LID is
// not used. An InfiniBand port takes the LID the subnet manager assigned the
// group, 0xC000 or more, which the test does not have.
func TestUDMulticastAttachDetach(t *testing.T) {
	p := newUDPair(t)
	pa, err := p.ctx.QueryPort(p.attr.Port)
	if err != nil {
		t.Fatalf("QueryPort(%d) error: %v", p.attr.Port, err)
	}
	if pa.LinkLayer != verbs.LinkLayerEthernet {
		t.Skipf("port %d's link layer is %v, not Ethernet: the kernel takes a multicast LID of 0xC000 or more there, which only a join through the subnet manager assigns", p.attr.Port, pa.LinkLayer)
	}
	group := verbs.GID(netip.MustParseAddr("::ffff:239.255.42.1").As16())
	if err := p.b.AttachMulticast(group, 0); err != nil {
		t.Fatalf("AttachMulticast(%v, 0) error: %v", group, err)
	}
	if err := p.b.DetachMulticast(group, 0); err != nil {
		t.Fatalf("DetachMulticast(%v, 0) error: %v", group, err)
	}

	err = p.b.DetachMulticast(group, 0)
	t.Logf("DetachMulticast(%v, 0) once detached: %v", group, err)
	if err != nil {
		checkOpError(t, err, "ibv_detach_mcast")
	}
	unicast := p.attr.GRH.DGID
	err = p.b.AttachMulticast(unicast, 0)
	t.Logf("AttachMulticast(%v, 0) of a unicast GID: %v", unicast, err)
	if err == nil {
		t.Errorf("AttachMulticast(%v, 0) of a unicast GID succeeded, want an error", unicast)
	} else {
		checkOpError(t, err, "ibv_attach_mcast")
	}
}
