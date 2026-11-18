//go:build linux && !android && (amd64 || arm64 || loong64 || ppc64le || riscv64 || s390x)

package verbs_test

import (
	"errors"
	"os"
	"strings"
	"syscall"
	"testing"

	"github.com/wzshiming/rdma/internal/rdmatest"
	"github.com/wzshiming/rdma/verbs"
)

// The software providers rxe and siw have no XRC: OpenXRCD reports
// EOPNOTSUPP, which matches errors.ErrUnsupported. An SRQ's number, which
// only XRC needs, may be missing as well.
func TestProviderWithoutXRCIsNotSupported(t *testing.T) {
	dev, ctx := openDevice(t)
	if !strings.HasPrefix(dev.Name, "rxe") && !strings.HasPrefix(dev.Name, "siw") {
		t.Skipf("%s is not rxe or siw, which have no XRC", dev.Name)
	}
	xrcd, err := ctx.OpenXRCD(-1, syscall.O_CREAT)
	var opErr *verbs.OpError
	if xrcd != nil || !errors.As(err, &opErr) || opErr.Op != "ibv_open_xrcd" || !errors.Is(err, syscall.EOPNOTSUPP) || !errors.Is(err, errors.ErrUnsupported) {
		t.Errorf("OpenXRCD() = %v, %v; want none and an *OpError of ibv_open_xrcd with EOPNOTSUPP", xrcd, err)
	}
	srq, _ := createSRQ(t, allocPD(t, ctx), 4)
	n, err := srq.Num()
	t.Logf("SRQ.Num() on %s: %d, %v", dev.Name, n, err)
	if err != nil && !errors.Is(err, errors.ErrUnsupported) {
		t.Errorf("SRQ.Num() error = %v, want nil or one matching errors.ErrUnsupported", err)
	}
}

// openXRCD opens a new XRC domain on ctx, closed with the test, or skips the
// test on a device without XRC, as rxe and siw are.
func openXRCD(t *testing.T, dev rdmatest.Dev, ctx *verbs.Context) *verbs.XRCD {
	t.Helper()
	xrcd, err := ctx.OpenXRCD(-1, syscall.O_CREAT)
	if errors.Is(err, errors.ErrUnsupported) {
		t.Skipf("%s has no XRC (%v): XRC needs a device with it, such as an mlx5 HCA", dev.Name, err)
	}
	if err != nil {
		t.Fatalf("OpenXRCD() error: %v", err)
	}
	t.Cleanup(func() {
		if err := xrcd.Close(); err != nil {
			t.Errorf("XRCD.Close() error: %v", err)
		}
	})
	return xrcd
}

// connectXRC drives an XRC queue pair from RESET to RTS with peer as its
// destination, with the attributes and masks of rdma-core's
// ibv_xsrq_pingpong: a receive queue pair takes no retry counts and no
// RDMA read depths, a send queue pair no RNR timer and no read depth of its
// responder.
func connectXRC(t *testing.T, p *rcPair, qp, peer *verbs.QP) {
	t.Helper()
	port := uint8(p.dev.Port)
	pa, err := p.ctx.QueryPort(port)
	if err != nil {
		t.Fatalf("QueryPort(%d) error: %v", port, err)
	}
	gids, err := p.ctx.GIDTable(port)
	if err != nil {
		t.Fatalf("GIDTable(%d) error: %v", port, err)
	}
	var gid verbs.GID
	for _, g := range gids {
		if g.Index == p.dev.GIDIndex {
			gid = g.GID
		}
	}
	recv := qp.Type() == verbs.QPTypeXRCRecv
	init := verbs.QPAttr{State: verbs.QPStateInit, Port: port}
	if recv {
		init.AccessFlags = verbs.AccessRemoteWrite | verbs.AccessRemoteRead
	}
	if err := qp.Modify(&init, verbs.QPAttrState|verbs.QPAttrPkeyIndex|verbs.QPAttrPort|verbs.QPAttrAccessFlags); err != nil {
		t.Fatalf("Modify(%v to INIT) error: %v", qp.Type(), err)
	}
	rtr := verbs.QPAttr{
		State:     verbs.QPStateRTR,
		PathMTU:   pa.ActiveMTU,
		DestQPNum: peer.Num(),
		AH: verbs.AHAttr{
			IsGlobal: !gid.IsZero(),
			DLID:     pa.LID,
			Port:     port,
			GRH:      verbs.GlobalRoute{DGID: gid, SGIDIndex: uint8(p.dev.GIDIndex), HopLimit: 64},
		},
	}
	rtrMask := verbs.QPAttrState | verbs.QPAttrAV | verbs.QPAttrPathMTU | verbs.QPAttrDestQPN | verbs.QPAttrRQPSN
	if recv {
		rtr.MinRNRTimer = 12
		rtrMask |= verbs.QPAttrMaxDestRDAtomic | verbs.QPAttrMinRNRTimer
	}
	if err := qp.Modify(&rtr, rtrMask); err != nil {
		t.Fatalf("Modify(%v to RTR) error: %v", qp.Type(), err)
	}
	rts := verbs.QPAttr{State: verbs.QPStateRTS, Timeout: 14}
	rtsMask := verbs.QPAttrState | verbs.QPAttrTimeout | verbs.QPAttrSQPSN
	if !recv {
		rts.RetryCnt, rts.RNRRetry = 7, 7
		rtsMask |= verbs.QPAttrRetryCnt | verbs.QPAttrRNRRetry | verbs.QPAttrMaxQPRDAtomic
	}
	if err := qp.Modify(&rts, rtsMask); err != nil {
		t.Fatalf("Modify(%v to RTS) error: %v", qp.Type(), err)
	}
}

// An XRC send queue pair sends to an XRC SRQ by its number: the message
// lands in a receive posted to the SRQ, which completes on the SRQ's CQ for
// the XRC receive queue pair the message came in on. On a device without
// XRC the test skips.
func TestXRCSendToAnSRQ(t *testing.T) {
	dev, ctx := openDevice(t)
	xrcd := openXRCD(t, dev, ctx)
	p := &rcPair{dev: dev, ctx: ctx, pd: allocPD(t, ctx)}
	sendCQ, recvCQ := createCQ(t, ctx, nil), createCQ(t, ctx, nil)
	attr := verbs.SRQInitAttr{Type: verbs.SRQTypeXRC, Attr: verbs.SRQAttr{MaxWR: 8, MaxSGE: 1}, XRCD: xrcd, CQ: recvCQ}
	srq, err := p.pd.CreateSRQ(&attr)
	if err != nil {
		t.Fatalf("CreateSRQ(XRC) error: %v", err)
	}
	t.Cleanup(func() {
		if err := srq.Close(); err != nil {
			t.Errorf("SRQ.Close() error: %v", err)
		}
	})
	srqn, err := srq.Num()
	if err != nil {
		t.Fatalf("SRQ.Num() error: %v", err)
	}
	recvQP := createXRCQP(t, p.pd, &verbs.QPInitAttr{Type: verbs.QPTypeXRCRecv, XRCD: xrcd})
	sendQP := createXRCQP(t, p.pd, &verbs.QPInitAttr{Type: verbs.QPTypeXRCSend, SendCQ: sendCQ, Cap: verbs.QPCap{MaxSendWR: 8, MaxSendSGE: 1}})
	connectXRC(t, p, recvQP, sendQP)
	connectXRC(t, p, sendQP, recvQP)

	mr := allocMR(t, p.pd, 4096)
	buf := mr.Bytes()
	if err := srq.PostRecv(verbs.RecvWR{ID: 7, SGE: []verbs.SGE{mr.SGE(buf[:64])}}); err != nil {
		t.Fatalf("SRQ.PostRecv() error: %v", err)
	}
	msg := "through the XRC SRQ"
	copy(buf[1024:], msg)
	err = sendQP.PostSend(verbs.SendWR{ID: 9, Op: verbs.WRSend, Flags: verbs.SendSignaled, SGE: []verbs.SGE{mr.SGE(buf[1024 : 1024+len(msg)])}, RemoteSRQN: srqn})
	if err != nil {
		t.Fatalf("PostSend() to SRQ %#x error: %v", srqn, err)
	}
	if wc := pollOne(t, sendCQ); wc.ID != 9 {
		t.Errorf("send completion %+v, want request 9", wc)
	}
	wc := pollOne(t, recvCQ)
	if wc.ID != 7 || wc.Opcode != verbs.WCOpRecv || wc.QPNum != recvQP.Num() || string(buf[:wc.Len]) != msg {
		t.Errorf("receive completion %+v with %q, want receive 7 of %q on XRC receive queue pair %#x", wc, buf[:wc.Len], msg, recvQP.Num())
	}
}

// createXRCQP creates the XRC queue pair attr describes on pd, closed with
// the test.
func createXRCQP(t *testing.T, pd *verbs.PD, attr *verbs.QPInitAttr) *verbs.QP {
	t.Helper()
	qp, err := pd.CreateQP(attr)
	if err != nil {
		t.Fatalf("CreateQP(%v) error: %v", attr.Type, err)
	}
	t.Cleanup(func() {
		if err := qp.Close(); err != nil {
			t.Errorf("QP.Close() error: %v", err)
		}
	})
	return qp
}

// createXRCSRQ creates an XRC SRQ in xrcd for maxWR receives of one SGE,
// completing on cq, closed with the test, and returns it with its number.
func createXRCSRQ(t *testing.T, pd *verbs.PD, xrcd *verbs.XRCD, cq *verbs.CQ, maxWR uint32) (*verbs.SRQ, uint32) {
	t.Helper()
	attr := verbs.SRQInitAttr{Type: verbs.SRQTypeXRC, Attr: verbs.SRQAttr{MaxWR: maxWR, MaxSGE: 1}, XRCD: xrcd, CQ: cq}
	srq, err := pd.CreateSRQ(&attr)
	if err != nil {
		t.Fatalf("CreateSRQ(XRC) error: %v", err)
	}
	t.Cleanup(func() {
		if err := srq.Close(); err != nil {
			t.Errorf("SRQ.Close() error: %v", err)
		}
	})
	n, err := srq.Num()
	if err != nil {
		t.Fatalf("SRQ.Num() error: %v", err)
	}
	return srq, n
}

// Two device contexts, as two processes have, share the XRC domain they open
// through the same file: O_EXCL refuses the second open, and a message that
// comes in through an XRC receive queue pair in the domain of one context
// lands in an XRC SRQ in the domain of the other. On a device without XRC
// the test skips.
func TestXRCDomainSharedThroughAFile(t *testing.T) {
	dev, ctx := openDevice(t)
	_, other := openDevice(t)
	f, err := os.CreateTemp(t.TempDir(), "xrcd")
	if err != nil {
		t.Fatalf("CreateTemp() error: %v", err)
	}
	t.Cleanup(func() { f.Close() })
	open := func(ctx *verbs.Context, oflags int) (*verbs.XRCD, error) {
		xrcd, err := ctx.OpenXRCD(int(f.Fd()), oflags)
		if err == nil {
			t.Cleanup(func() {
				if err := xrcd.Close(); err != nil {
					t.Errorf("XRCD.Close() error: %v", err)
				}
			})
		}
		return xrcd, err
	}
	xrcd, err := open(ctx, syscall.O_CREAT|syscall.O_EXCL)
	if errors.Is(err, errors.ErrUnsupported) {
		t.Skipf("%s has no XRC (%v): XRC needs a device with it, such as an mlx5 HCA", dev.Name, err)
	}
	if err != nil {
		t.Fatalf("OpenXRCD(O_CREAT|O_EXCL) of a new file error: %v", err)
	}
	if _, err := open(other, syscall.O_CREAT|syscall.O_EXCL); err == nil {
		t.Error("OpenXRCD(O_CREAT|O_EXCL) opened the existing domain of the file")
	} else {
		t.Logf("OpenXRCD(O_CREAT|O_EXCL) of the existing domain: %v", err)
	}
	shared, err := open(other, 0)
	if err != nil {
		t.Fatalf("OpenXRCD() of the existing domain of the file error: %v", err)
	}

	p := &rcPair{dev: dev, ctx: ctx, pd: allocPD(t, ctx)}
	sendCQ, recvCQ := createCQ(t, ctx, nil), createCQ(t, ctx, nil)
	srq, srqn := createXRCSRQ(t, p.pd, xrcd, recvCQ, 8)
	recvQP := createXRCQP(t, allocPD(t, other), &verbs.QPInitAttr{Type: verbs.QPTypeXRCRecv, XRCD: shared})
	sendQP := createXRCQP(t, p.pd, &verbs.QPInitAttr{Type: verbs.QPTypeXRCSend, SendCQ: sendCQ, Cap: verbs.QPCap{MaxSendWR: 8, MaxSendSGE: 1}})
	connectXRC(t, p, recvQP, sendQP)
	connectXRC(t, p, sendQP, recvQP)

	mr := allocMR(t, p.pd, 4096)
	buf := mr.Bytes()
	if err := srq.PostRecv(verbs.RecvWR{ID: 7, SGE: []verbs.SGE{mr.SGE(buf[:64])}}); err != nil {
		t.Fatalf("SRQ.PostRecv() error: %v", err)
	}
	msg := "through the shared XRC domain"
	copy(buf[1024:], msg)
	err = sendQP.PostSend(verbs.SendWR{ID: 9, Op: verbs.WRSend, Flags: verbs.SendSignaled, SGE: []verbs.SGE{mr.SGE(buf[1024 : 1024+len(msg)])}, RemoteSRQN: srqn})
	if err != nil {
		t.Fatalf("PostSend() to SRQ %#x error: %v", srqn, err)
	}
	if wc := pollOne(t, sendCQ); wc.ID != 9 {
		t.Errorf("send completion %+v, want request 9", wc)
	}
	wc := pollOne(t, recvCQ)
	if wc.ID != 7 || wc.Opcode != verbs.WCOpRecv || wc.QPNum != recvQP.Num() || string(buf[:wc.Len]) != msg {
		t.Errorf("receive completion %+v with %q, want receive 7 of %q on the other context's XRC receive queue pair %#x", wc, buf[:wc.Len], msg, recvQP.Num())
	}
}

// A basic SRQ has a number as well, on a device whose provider gives one, as
// mlx5's does, and two SRQs have different numbers. On rxe and siw, whose
// providers leave get_srq_num to libibverbs's stub, the test skips.
func TestBasicSRQNum(t *testing.T) {
	dev, ctx := openDevice(t)
	pd := allocPD(t, ctx)
	var nums []uint32
	for range 2 {
		srq, _ := createSRQ(t, pd, 4)
		n, err := srq.Num()
		if errors.Is(err, errors.ErrUnsupported) {
			t.Skipf("%s gives no SRQ numbers (%v): that needs a provider with get_srq_num, such as mlx5's", dev.Name, err)
		}
		if err != nil {
			t.Fatalf("SRQ.Num() error: %v", err)
		}
		nums = append(nums, n)
	}
	t.Logf("SRQ numbers on %s: %#x", dev.Name, nums)
	if nums[0] == nums[1] || nums[0] > 0xffffff || nums[1] > 0xffffff {
		t.Errorf("SRQ numbers %#x, want two different numbers of 24 bits", nums)
	}
}
