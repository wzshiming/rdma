//go:build linux && !android && (amd64 || arm64 || loong64 || ppc64le || riscv64 || s390x)

package verbs_test

import (
	"bytes"
	"context"
	"errors"
	"os"
	"runtime"
	"strconv"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/wzshiming/rdma/internal/rdmatest"
	"github.com/wzshiming/rdma/verbs"
)

// rcPair is two RC queue pairs on the rdmatest device connected to each other
// through the device's own GID; a and b each complete on their own CQ, and
// both CQs share one completion channel.
type rcPair struct {
	dev      rdmatest.Dev
	ctx      *verbs.Context
	pd       *verbs.PD
	ch       *verbs.CompChannel
	cqA, cqB *verbs.CQ
	a, b     *verbs.QP
	inline   uint32 // inline data size granted by the device
}

func newRCPair(t *testing.T) *rcPair {
	t.Helper()
	dev, ctx := openDevice(t)
	p := &rcPair{dev: dev, ctx: ctx, pd: allocPD(t, ctx)}
	var err error
	if p.ch, err = ctx.CreateCompChannel(); err != nil {
		t.Fatalf("CreateCompChannel() error: %v", err)
	}
	// Registered before the CQs so that it runs after they are gone.
	t.Cleanup(func() {
		if err := p.ch.Close(); err != nil {
			t.Errorf("CompChannel.Close() error: %v", err)
		}
	})
	p.cqA = createCQ(t, ctx, p.ch)
	p.cqB = createCQ(t, ctx, p.ch)
	p.a, p.inline = createRC(t, p.pd, p.cqA)
	p.b, _ = createRC(t, p.pd, p.cqB)
	p.connect(t, p.a, p.b)
	p.connect(t, p.b, p.a)
	return p
}

func createCQ(t *testing.T, ctx *verbs.Context, ch *verbs.CompChannel) *verbs.CQ {
	t.Helper()
	cq, err := ctx.CreateCQ(64, ch)
	if err != nil {
		t.Fatalf("CreateCQ() error: %v", err)
	}
	t.Cleanup(func() {
		if err := cq.Close(); err != nil {
			t.Errorf("CQ.Close() error: %v", err)
		}
	})
	return cq
}

func createRC(t *testing.T, pd *verbs.PD, cq *verbs.CQ) (*verbs.QP, uint32) {
	t.Helper()
	attr := verbs.QPInitAttr{
		SendCQ: cq,
		RecvCQ: cq,
		Type:   verbs.QPTypeRC,
		Cap:    verbs.QPCap{MaxSendWR: 32, MaxRecvWR: 32, MaxSendSGE: 2, MaxRecvSGE: 2, MaxInlineData: 64},
	}
	qp, err := pd.CreateQP(&attr)
	if err != nil {
		t.Fatalf("CreateQP() error: %v", err)
	}
	t.Cleanup(func() {
		if err := qp.Close(); err != nil {
			t.Errorf("QP.Close() error: %v", err)
		}
	})
	if attr.Cap.MaxSendWR < 32 || attr.Cap.MaxRecvWR < 32 || attr.Cap.MaxSendSGE < 2 || attr.Cap.MaxRecvSGE < 2 {
		t.Fatalf("CreateQP() granted %+v, want at least the requested capacities", attr.Cap)
	}
	return qp, attr.Cap.MaxInlineData
}

// connect drives qp from RESET to RTS with peer as its destination.
func (p *rcPair) connect(t *testing.T, qp, peer *verbs.QP) {
	t.Helper()
	port := uint8(p.dev.Port)
	pa, err := p.ctx.QueryPort(port)
	if err != nil {
		t.Fatalf("QueryPort(%d) error: %v", port, err)
	}
	var gid verbs.GID
	gids, err := p.ctx.GIDTable(port)
	if err != nil {
		t.Fatalf("GIDTable(%d) error: %v", port, err)
	}
	for _, g := range gids {
		if g.Index == p.dev.GIDIndex {
			gid = g.GID
		}
	}
	if gid.IsZero() {
		t.Fatalf("GIDTable(%d) has no entry at index %d", port, p.dev.GIDIndex)
	}

	err = qp.Modify(&verbs.QPAttr{
		State:       verbs.QPStateInit,
		PkeyIndex:   0,
		Port:        port,
		AccessFlags: verbs.AccessLocalWrite | verbs.AccessRemoteWrite | verbs.AccessRemoteRead,
	}, verbs.QPAttrState|verbs.QPAttrPkeyIndex|verbs.QPAttrPort|verbs.QPAttrAccessFlags)
	if err != nil {
		t.Fatalf("Modify(INIT) error: %v", err)
	}
	err = qp.Modify(&verbs.QPAttr{
		State:           verbs.QPStateRTR,
		PathMTU:         pa.ActiveMTU,
		DestQPNum:       peer.Num(),
		RQPSN:           0,
		MaxDestRDAtomic: 1,
		MinRNRTimer:     12,
		AH: verbs.AHAttr{
			IsGlobal: true,
			DLID:     pa.LID,
			Port:     port,
			GRH:      verbs.GlobalRoute{DGID: gid, SGIDIndex: uint8(p.dev.GIDIndex), HopLimit: 64},
		},
	}, verbs.QPAttrState|verbs.QPAttrAV|verbs.QPAttrPathMTU|verbs.QPAttrDestQPN|verbs.QPAttrRQPSN|verbs.QPAttrMaxDestRDAtomic|verbs.QPAttrMinRNRTimer)
	if err != nil {
		t.Fatalf("Modify(RTR) error: %v", err)
	}
	err = qp.Modify(&verbs.QPAttr{
		State:       verbs.QPStateRTS,
		Timeout:     14,
		RetryCnt:    7,
		RNRRetry:    7,
		SQPSN:       0,
		MaxRDAtomic: 1,
	}, verbs.QPAttrState|verbs.QPAttrTimeout|verbs.QPAttrRetryCnt|verbs.QPAttrRNRRetry|verbs.QPAttrSQPSN|verbs.QPAttrMaxQPRDAtomic)
	if err != nil {
		t.Fatalf("Modify(RTS) error: %v", err)
	}
}

func allocMR(t *testing.T, pd *verbs.PD, size int) *verbs.MR {
	t.Helper()
	mr, err := pd.AllocMemory(size, verbs.AccessLocalWrite|verbs.AccessRemoteWrite|verbs.AccessRemoteRead)
	if err != nil {
		t.Fatalf("AllocMemory(%d) error: %v", size, err)
	}
	t.Cleanup(func() {
		if err := mr.Close(); err != nil {
			t.Errorf("MR.Close() error: %v", err)
		}
	})
	return mr
}

// pollN polls cq until it has collected n successful completions, in the
// order they were polled; completions may arrive in one batch or several.
func pollN(t *testing.T, cq *verbs.CQ, n int) []verbs.WC {
	t.Helper()
	var got []verbs.WC
	buf := make([]verbs.WC, n+1)
	deadline := time.Now().Add(10 * time.Second)
	for time.Now().Before(deadline) {
		k, err := cq.Poll(buf)
		if err != nil {
			t.Fatalf("Poll() error: %v", err)
		}
		for _, wc := range buf[:k] {
			if err := wc.Err(); err != nil {
				t.Fatalf("completion %v of request %d failed: %v", wc.Opcode, wc.ID, err)
			}
		}
		got = append(got, buf[:k]...)
		if len(got) > n {
			t.Fatalf("Poll() yielded %d completions, want %d: %+v", len(got), n, got)
		}
		if len(got) == n {
			return got
		}
		time.Sleep(100 * time.Microsecond)
	}
	t.Fatalf("Poll() yielded %d of %d completions within 10s: %+v", len(got), n, got)
	return nil
}

// pollOne polls cq until it yields one successful completion.
func pollOne(t *testing.T, cq *verbs.CQ) verbs.WC {
	t.Helper()
	return pollN(t, cq, 1)[0]
}

func pattern(b []byte, seed byte) {
	for i := range b {
		b[i] = seed + byte(i*7)
	}
}

func TestRCLoopback(t *testing.T) {
	p := newRCPair(t)
	mrA := allocMR(t, p.pd, 8192)
	mrB := allocMR(t, p.pd, 8192)
	bufA, bufB := mrA.Bytes(), mrB.Bytes()

	t.Run("Query", func(t *testing.T) {
		attr, err := p.a.Query(verbs.QPAttrState | verbs.QPAttrPathMTU | verbs.QPAttrCap | verbs.QPAttrDestQPN)
		if err != nil {
			t.Fatalf("Query() error: %v", err)
		}
		if attr.State != verbs.QPStateRTS || attr.DestQPNum != p.b.Num() || attr.Cap.MaxSendWR < 32 {
			t.Errorf("Query() = state %v, dest %d, cap %+v; want RTS, %d, >= 32 send WRs", attr.State, attr.DestQPNum, attr.Cap, p.b.Num())
		}
		if p.a.Type() != verbs.QPTypeRC || p.a.Num() == p.b.Num() {
			t.Errorf("Type() = %v, Num() = %d/%d", p.a.Type(), p.a.Num(), p.b.Num())
		}
	})

	t.Run("SendRecv", func(t *testing.T) {
		msg := []byte("hello over soft-roce")
		clear(bufB[:64])
		if err := p.b.PostRecv(verbs.RecvWR{ID: 1, SGE: []verbs.SGE{mrB.SGE(bufB[:64])}}); err != nil {
			t.Fatalf("PostRecv() error: %v", err)
		}
		copy(bufA, msg)
		if err := p.a.PostSend(verbs.SendWR{ID: 2, Op: verbs.WRSend, SGE: []verbs.SGE{mrA.SGE(bufA[:len(msg)])}, Flags: verbs.SendSignaled}); err != nil {
			t.Fatalf("PostSend() error: %v", err)
		}
		recv := pollOne(t, p.cqB)
		if recv.ID != 1 || recv.Opcode != verbs.WCOpRecv || int(recv.Len) != len(msg) || recv.HasImm() || recv.QPNum != p.b.Num() {
			t.Errorf("receive completion = %+v, want id 1, RECV, %d bytes on QP %d", recv, len(msg), p.b.Num())
		}
		send := pollOne(t, p.cqA)
		if send.ID != 2 || send.Opcode != verbs.WCOpSend {
			t.Errorf("send completion = %+v, want id 2, SEND", send)
		}
		if !bytes.Equal(bufB[:len(msg)], msg) {
			t.Errorf("received %q, want %q", bufB[:len(msg)], msg)
		}
	})

	t.Run("SendImm", func(t *testing.T) {
		if err := p.b.PostRecv(verbs.RecvWR{ID: 3, SGE: []verbs.SGE{mrB.SGE(bufB[:64])}}); err != nil {
			t.Fatalf("PostRecv() error: %v", err)
		}
		if err := p.a.PostSend(verbs.SendWR{ID: 4, Op: verbs.WRSendImm, Imm: 0xdeadbeef, SGE: []verbs.SGE{mrA.SGE(bufA[:8])}, Flags: verbs.SendSignaled}); err != nil {
			t.Fatalf("PostSend() error: %v", err)
		}
		recv := pollOne(t, p.cqB)
		if recv.ID != 3 || recv.Opcode != verbs.WCOpRecv || !recv.HasImm() || recv.Imm != 0xdeadbeef || recv.Len != 8 {
			t.Errorf("receive completion = %+v, want id 3, RECV with imm 0xdeadbeef, 8 bytes", recv)
		}
		pollOne(t, p.cqA)
	})

	t.Run("Write", func(t *testing.T) {
		pattern(bufA[:1000], 1)
		clear(bufB[:1000])
		wr := verbs.SendWR{ID: 5, Op: verbs.WRWrite, SGE: []verbs.SGE{mrA.SGE(bufA[:1000])}, Flags: verbs.SendSignaled, RemoteAddr: mrB.SGE(bufB).Addr, RKey: mrB.RKey()}
		if err := p.a.PostSend(wr); err != nil {
			t.Fatalf("PostSend() error: %v", err)
		}
		if wc := pollOne(t, p.cqA); wc.ID != 5 || wc.Opcode != verbs.WCOpRDMAWrite {
			t.Errorf("write completion = %+v, want id 5, RDMA_WRITE", wc)
		}
		if !bytes.Equal(bufB[:1000], bufA[:1000]) {
			t.Error("RDMA Write did not transfer the bytes")
		}
	})

	t.Run("WriteImm", func(t *testing.T) {
		pattern(bufA[:300], 2)
		clear(bufB[:300])
		if err := p.b.PostRecv(verbs.RecvWR{ID: 6}); err != nil {
			t.Fatalf("PostRecv() error: %v", err)
		}
		wr := verbs.SendWR{ID: 7, Op: verbs.WRWriteImm, Imm: 42, SGE: []verbs.SGE{mrA.SGE(bufA[:300])}, Flags: verbs.SendSignaled, RemoteAddr: mrB.SGE(bufB).Addr, RKey: mrB.RKey()}
		if err := p.a.PostSend(wr); err != nil {
			t.Fatalf("PostSend() error: %v", err)
		}
		recv := pollOne(t, p.cqB)
		if recv.ID != 6 || recv.Opcode != verbs.WCOpRecvRDMAWithImm || !recv.HasImm() || recv.Imm != 42 || recv.Len != 300 {
			t.Errorf("receive completion = %+v, want id 6, RECV_RDMA_WITH_IMM, imm 42, 300 bytes", recv)
		}
		pollOne(t, p.cqA)
		if !bytes.Equal(bufB[:300], bufA[:300]) {
			t.Error("RDMA Write with immediate did not transfer the bytes")
		}
	})

	t.Run("Read", func(t *testing.T) {
		pattern(bufB[:1000], 3)
		clear(bufA[:1000])
		wr := verbs.SendWR{ID: 8, Op: verbs.WRRead, SGE: []verbs.SGE{mrA.SGE(bufA[:1000])}, Flags: verbs.SendSignaled, RemoteAddr: mrB.SGE(bufB).Addr, RKey: mrB.RKey()}
		if err := p.a.PostSend(wr); err != nil {
			t.Fatalf("PostSend() error: %v", err)
		}
		if wc := pollOne(t, p.cqA); wc.ID != 8 || wc.Opcode != verbs.WCOpRDMARead {
			t.Errorf("read completion = %+v, want id 8, RDMA_READ", wc)
		}
		if !bytes.Equal(bufA[:1000], bufB[:1000]) {
			t.Error("RDMA Read did not transfer the bytes")
		}
	})

	t.Run("Inline", func(t *testing.T) {
		if p.inline == 0 {
			t.Skip("device grants no inline data")
		}
		n := int(min(p.inline, 32))
		msg := bytes.Repeat([]byte{0xa5}, n)
		clear(bufB[:64])
		if err := p.b.PostRecv(verbs.RecvWR{ID: 9, SGE: []verbs.SGE{mrB.SGE(bufB[:64])}}); err != nil {
			t.Fatalf("PostRecv() error: %v", err)
		}
		copy(bufA, msg)
		if err := p.a.PostSend(verbs.SendWR{ID: 10, Op: verbs.WRSend, SGE: []verbs.SGE{mrA.SGE(bufA[:n])}, Flags: verbs.SendSignaled | verbs.SendInline}); err != nil {
			t.Fatalf("PostSend(inline) error: %v", err)
		}
		clear(bufA[:n]) // inline data is copied at post time, so the source may be reused at once
		recv := pollOne(t, p.cqB)
		if recv.ID != 9 || int(recv.Len) != n {
			t.Errorf("receive completion = %+v, want id 9, %d bytes", recv, n)
		}
		pollOne(t, p.cqA)
		if !bytes.Equal(bufB[:n], msg) {
			t.Errorf("received %x, want %x", bufB[:n], msg)
		}
	})

	t.Run("MultipleWRs", func(t *testing.T) {
		clear(bufB[:64])
		recvs := []verbs.RecvWR{
			{ID: 11, SGE: []verbs.SGE{mrB.SGE(bufB[:16])}},
			{ID: 12, SGE: []verbs.SGE{mrB.SGE(bufB[16:24]), mrB.SGE(bufB[24:32])}},
		}
		if err := p.b.PostRecv(recvs...); err != nil {
			t.Fatalf("PostRecv() error: %v", err)
		}
		pattern(bufA[:32], 4)
		sends := []verbs.SendWR{
			{ID: 13, Op: verbs.WRSend, SGE: []verbs.SGE{mrA.SGE(bufA[:16])}, Flags: verbs.SendSignaled},
			{ID: 14, Op: verbs.WRSend, SGE: []verbs.SGE{mrA.SGE(bufA[16:24]), mrA.SGE(bufA[24:32])}, Flags: verbs.SendSignaled},
		}
		if err := p.a.PostSend(sends...); err != nil {
			t.Fatalf("PostSend() error: %v", err)
		}
		recvs2 := pollN(t, p.cqB, 2)
		if recvs2[0].ID != 11 || recvs2[0].Len != 16 || recvs2[1].ID != 12 || recvs2[1].Len != 16 {
			t.Errorf("receive completions = %+v, want ids 11 and 12 with 16 bytes each", recvs2)
		}
		if sends2 := pollN(t, p.cqA, 2); sends2[0].ID != 13 || sends2[1].ID != 14 {
			t.Errorf("send completions = %+v, want ids 13 and 14", sends2)
		}
		if !bytes.Equal(bufB[:32], bufA[:32]) {
			t.Error("chained work requests did not transfer the bytes")
		}
	})

	t.Run("CompChannelWait", func(t *testing.T) {
		if err := p.cqB.RequestNotify(false); err != nil {
			t.Fatalf("RequestNotify() error: %v", err)
		}
		if err := p.b.PostRecv(verbs.RecvWR{ID: 15, SGE: []verbs.SGE{mrB.SGE(bufB[:64])}}); err != nil {
			t.Fatalf("PostRecv() error: %v", err)
		}
		if err := p.a.PostSend(verbs.SendWR{ID: 16, Op: verbs.WRSend, SGE: []verbs.SGE{mrA.SGE(bufA[:8])}, Flags: verbs.SendSignaled}); err != nil {
			t.Fatalf("PostSend() error: %v", err)
		}
		ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
		defer cancel()
		cq, err := p.ch.Wait(ctx)
		if err != nil {
			t.Fatalf("Wait() error: %v", err)
		}
		if cq != p.cqB {
			t.Errorf("Wait() = CQ %p, want the notified CQ %p", cq, p.cqB)
		}
		if wc := pollOne(t, cq); wc.ID != 15 {
			t.Errorf("receive completion = %+v, want id 15", wc)
		}
		pollOne(t, p.cqA)

		// The event was acknowledged, so an idle wait only ends with the context.
		ctx2, cancel2 := context.WithTimeout(context.Background(), 200*time.Millisecond)
		defer cancel2()
		if _, err := p.ch.Wait(ctx2); !errors.Is(err, context.DeadlineExceeded) {
			t.Errorf("idle Wait() error = %v, want context.DeadlineExceeded", err)
		}
	})

	t.Run("WriteImmZeroLength", func(t *testing.T) {
		if err := p.b.PostRecv(verbs.RecvWR{ID: 17}); err != nil {
			t.Fatalf("PostRecv() error: %v", err)
		}
		// As rsockets sends a control message (RS_OP_CTRL, RS_CTRL_SHUTDOWN): no SGE, remote address or key.
		if err := p.a.PostSend(verbs.SendWR{ID: 18, Op: verbs.WRWriteImm, Imm: 0xe0000002, Flags: verbs.SendSignaled}); err != nil {
			t.Fatalf("PostSend() error: %v", err)
		}
		recv := pollOne(t, p.cqB)
		t.Logf("receive completion: %+v", recv)
		if recv.ID != 17 || recv.Opcode != verbs.WCOpRecvRDMAWithImm || !recv.HasImm() || recv.Imm != 0xe0000002 || recv.Len != 0 {
			t.Errorf("receive completion = %+v, want id 17, RECV_RDMA_WITH_IMM, imm 0xe0000002, 0 bytes", recv)
		}
		send := pollOne(t, p.cqA)
		t.Logf("send completion: %+v", send)
		if send.ID != 18 || send.Opcode != verbs.WCOpRDMAWrite {
			t.Errorf("send completion = %+v, want id 18, RDMA_WRITE", send)
		}
	})
}

func TestCompChannelWaitCancel(t *testing.T) {
	_, ctx := openDevice(t)
	ch, err := ctx.CreateCompChannel()
	if err != nil {
		t.Fatalf("CreateCompChannel() error: %v", err)
	}
	if ch.Pointer() == nil {
		t.Error("Pointer() = nil")
	}

	wctx, cancel := context.WithTimeout(context.Background(), 200*time.Millisecond)
	defer cancel()
	start := time.Now()
	cq, err := ch.Wait(wctx)
	if cq != nil || !errors.Is(err, context.DeadlineExceeded) {
		t.Errorf("Wait() = %v, %v; want nil, context.DeadlineExceeded", cq, err)
	}
	if elapsed := time.Since(start); elapsed > 2*time.Second {
		t.Errorf("Wait() returned after %v, want within 2s", elapsed)
	}

	cancelled, cancelNow := context.WithCancel(context.Background())
	cancelNow()
	if _, err := ch.Wait(cancelled); !errors.Is(err, context.Canceled) {
		t.Errorf("Wait(cancelled) error = %v, want context.Canceled", err)
	}

	// Closing the channel wakes a pending Wait with an error.
	errc := make(chan error, 1)
	go func() {
		_, err := ch.Wait(context.Background())
		errc <- err
	}()
	time.Sleep(100 * time.Millisecond)
	if err := ch.Close(); err != nil {
		t.Fatalf("Close() error: %v", err)
	}
	select {
	case err := <-errc:
		if err == nil {
			t.Error("Wait() on a closed channel returned nil error")
		}
	case <-time.After(5 * time.Second):
		t.Error("Wait() did not return after Close()")
	}
}

// registerFresh registers a slice that nothing but the MR references afterwards.
//
//go:noinline
func registerFresh(t *testing.T, pd *verbs.PD, size int, seed byte) *verbs.MR {
	t.Helper()
	buf := make([]byte, size)
	pattern(buf, seed)
	mr, err := pd.RegisterMemory(buf, verbs.AccessLocalWrite|verbs.AccessRemoteWrite|verbs.AccessRemoteRead)
	if err != nil {
		t.Fatalf("RegisterMemory() error: %v", err)
	}
	return mr
}

func TestRegisteredSliceSurvivesGC(t *testing.T) {
	p := newRCPair(t)
	const size = 4096
	mrB := registerFresh(t, p.pd, size, 5)
	defer mrB.Close()
	for i := 0; i < 5; i++ {
		runtime.GC()
	}
	want := make([]byte, size)
	pattern(want, 5)
	if !bytes.Equal(mrB.Bytes(), want) {
		t.Fatal("registered slice changed across GC cycles")
	}

	mrA := allocMR(t, p.pd, size)
	pattern(mrA.Bytes(), 6)
	pattern(want, 6)
	wr := verbs.SendWR{ID: 1, Op: verbs.WRWrite, SGE: []verbs.SGE{mrA.SGE(mrA.Bytes())}, Flags: verbs.SendSignaled, RemoteAddr: mrB.SGE(mrB.Bytes()).Addr, RKey: mrB.RKey()}
	if err := p.a.PostSend(wr); err != nil {
		t.Fatalf("PostSend() error: %v", err)
	}
	pollOne(t, p.cqA)
	for i := 0; i < 3; i++ {
		runtime.GC()
	}
	if !bytes.Equal(mrB.Bytes(), want) {
		t.Error("RDMA Write into the registered Go slice did not land in MR.Bytes()")
	}
}

func threadCount(t *testing.T) int {
	t.Helper()
	status, err := os.ReadFile("/proc/self/status")
	if err != nil {
		t.Fatalf("read /proc/self/status: %v", err)
	}
	for _, line := range strings.Split(string(status), "\n") {
		if n, ok := strings.CutPrefix(line, "Threads:"); ok {
			c, err := strconv.Atoi(strings.TrimSpace(n))
			if err != nil {
				t.Fatalf("parse %q: %v", line, err)
			}
			return c
		}
	}
	t.Fatal("no Threads line in /proc/self/status")
	return 0
}

func TestWaitDoesNotHoldThreads(t *testing.T) {
	_, ctx := openDevice(t)
	const waiters = 100
	chs := make([]*verbs.CompChannel, waiters)
	for i := range chs {
		ch, err := ctx.CreateCompChannel()
		if err != nil {
			t.Fatalf("CreateCompChannel() #%d error: %v", i, err)
		}
		defer ch.Close()
		chs[i] = ch
	}

	before := threadCount(t)
	wctx, cancel := context.WithCancel(context.Background())
	var wg sync.WaitGroup
	errs := make(chan error, waiters)
	for _, ch := range chs {
		wg.Add(1)
		go func() {
			defer wg.Done()
			_, err := ch.Wait(wctx)
			errs <- err
		}()
	}
	time.Sleep(500 * time.Millisecond)
	after := threadCount(t)
	cancel()
	wg.Wait()
	close(errs)
	for err := range errs {
		if !errors.Is(err, context.Canceled) {
			t.Errorf("Wait() error = %v, want context.Canceled", err)
		}
	}
	if after-before >= 10 {
		t.Errorf("Threads grew from %d to %d with %d idle waits, want growth < 10", before, after, waiters)
	}
}

func TestAsyncEvent(t *testing.T) {
	p := newRCPair(t)

	idle, cancelIdle := context.WithTimeout(context.Background(), 200*time.Millisecond)
	defer cancelIdle()
	if ev, err := p.ctx.WaitAsyncEvent(idle); !errors.Is(err, context.DeadlineExceeded) {
		t.Fatalf("idle WaitAsyncEvent() = %+v, %v; want context.DeadlineExceeded", ev, err)
	}

	// Draining an idle send queue raises SQ_DRAINED for the queue pair.
	err := p.a.Modify(&verbs.QPAttr{State: verbs.QPStateSQD, EnSQDAsyncNotify: true}, verbs.QPAttrState|verbs.QPAttrEnSQDAsyncNotify)
	if err != nil {
		t.Fatalf("Modify(SQD) error: %v", err)
	}
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()
	ev, err := p.ctx.WaitAsyncEvent(ctx)
	if err != nil {
		t.Fatalf("WaitAsyncEvent() error: %v", err)
	}
	if ev.Type != verbs.EventSQDrained || ev.QP != p.a {
		t.Errorf("WaitAsyncEvent() = {Type %v, QP %p}, want SQ_DRAINED on QP %p", ev.Type, ev.QP, p.a)
	}
}
