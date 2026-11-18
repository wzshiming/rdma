//go:build linux && !android && (amd64 || arm64 || loong64 || ppc64le || riscv64 || s390x)

package verbs

import (
	"errors"
	"os"
	"slices"
	"sync"
	"sync/atomic"
	"syscall"
	"testing"
	"unsafe"

	"github.com/ebitengine/purego"

	"github.com/wzshiming/rdma/internal/dynlib"
	"github.com/wzshiming/rdma/internal/rdmaabi"
)

// xrcProvider is a provider with XRC: the ops of a fake struct verbs_context
// are Go functions that record what they are called with and hand out a
// domain, an SRQ and a queue pair in C memory.
type xrcProvider struct {
	ctx  *rdmaabi.IbvContext
	xrcd *rdmaabi.IbvXrcd
	srq  *rdmaabi.IbvSrq
	qp   *rdmaabi.IbvQp

	openCtx  uintptr
	openAttr rdmaabi.IbvXrcdInitAttr
	srqCtx   uintptr
	srqAttr  rdmaabi.IbvSrqInitAttrEx
	numSRQ   uintptr
	qpCtx    uintptr
	qpAttr   rdmaabi.IbvQpInitAttrEx
	qpCalls  int
	closed   []uintptr
}

// fakeXRC is the provider whose ops the callbacks of xrcCallbacks run.
var fakeXRC atomic.Pointer[xrcProvider]

// xrcCallbacks makes the callbacks once: purego frees none, and a process
// has room for 2000.
var xrcCallbacks = sync.OnceValue(func() rdmaabi.VerbsContext {
	return rdmaabi.VerbsContext{
		Open_xrcd: uint64(purego.NewCallback(func(ctx, attr uintptr) uintptr {
			return fakeXRC.Load().openXRCD(ctx, attr)
		})),
		Close_xrcd: uint64(purego.NewCallback(func(xrcd uintptr) uintptr {
			return fakeXRC.Load().closeXRCD(xrcd)
		})),
		Create_srq_ex: uint64(purego.NewCallback(func(ctx, attr uintptr) uintptr {
			return fakeXRC.Load().createSRQEx(ctx, attr)
		})),
		Get_srq_num: uint64(purego.NewCallback(func(srq, num uintptr) uintptr {
			return fakeXRC.Load().getSRQNum(srq, num)
		})),
		Create_qp_ex: uint64(purego.NewCallback(func(ctx, attr uintptr) uintptr {
			return fakeXRC.Load().createQPEx(ctx, attr)
		})),
	}
})

// newXRCProvider returns the provider, whose ops answer until the test ends.
func newXRCProvider(t *testing.T) *xrcProvider {
	t.Helper()
	ctx, vctx := fakeVerbsContext(t)
	p := &xrcProvider{
		ctx:  ctx,
		xrcd: (*rdmaabi.IbvXrcd)(cMemory(t, os.Getpagesize())),
		srq:  (*rdmaabi.IbvSrq)(cMemory(t, os.Getpagesize())),
		qp:   (*rdmaabi.IbvQp)(cMemory(t, os.Getpagesize())),
	}
	// The objects name the context whatever the ops are called with.
	p.xrcd.Context, p.srq.Context, p.qp.Context = uint64(caddr(ctx)), uint64(caddr(ctx)), uint64(caddr(ctx))
	ops := xrcCallbacks()
	vctx.Open_xrcd, vctx.Close_xrcd = ops.Open_xrcd, ops.Close_xrcd
	vctx.Create_srq_ex, vctx.Get_srq_num, vctx.Create_qp_ex = ops.Create_srq_ex, ops.Get_srq_num, ops.Create_qp_ex
	fakeXRC.Store(p)
	t.Cleanup(func() { fakeXRC.Store(nil) })
	return p
}

func (p *xrcProvider) openXRCD(ctx, attr uintptr) uintptr {
	p.openCtx, p.openAttr = ctx, *dynlib.Pointer[rdmaabi.IbvXrcdInitAttr](attr)
	return caddr(p.xrcd)
}

func (p *xrcProvider) closeXRCD(xrcd uintptr) uintptr {
	p.closed = append(p.closed, xrcd)
	return 0
}

// createSRQEx grants 15 receives, as a provider rounds up the 8 asked for.
func (p *xrcProvider) createSRQEx(ctx, attr uintptr) uintptr {
	a := dynlib.Pointer[rdmaabi.IbvSrqInitAttrEx](attr)
	p.srqCtx, p.srqAttr = ctx, *a
	a.Attr.Max_wr = 15
	return caddr(p.srq)
}

func (p *xrcProvider) getSRQNum(srq, num uintptr) uintptr {
	p.numSRQ = srq
	*dynlib.Pointer[uint32](num) = 0xabcdef
	return 0
}

// createQPEx grants 64 bytes of inline data, which is how a provider
// reports its maximum.
func (p *xrcProvider) createQPEx(ctx, attr uintptr) uintptr {
	a := dynlib.Pointer[rdmaabi.IbvQpInitAttrEx](attr)
	p.qpCtx, p.qpAttr = ctx, *a
	p.qpCalls++
	a.Cap.Max_inline_data = 64
	p.qp.Qp_type = a.Qp_type
	return caddr(p.qp)
}

// The XRC verbs call the provider's ops as libibverbs's inline wrappers do:
// open_xrcd, create_srq_ex and create_qp_ex with the context and the whole
// init attributes, get_srq_num with the SRQ, close_xrcd with the domain; what
// the provider grants comes back, and the SRQ and the queue pair are known
// to the events.
func TestXRCVerbsCallTheProvider(t *testing.T) {
	p := newXRCProvider(t)
	ctx := caddr(p.ctx)
	xrcd, err := ContextFromPointer(unsafe.Pointer(p.ctx)).OpenXRCD(5, syscall.O_CREAT|syscall.O_EXCL)
	if err != nil {
		t.Fatalf("OpenXRCD() error: %v", err)
	}
	wantOpen := rdmaabi.IbvXrcdInitAttr{Comp_mask: 1<<0 | 1<<1, Fd: 5, Oflags: syscall.O_CREAT | syscall.O_EXCL}
	if p.openCtx != ctx || p.openAttr != wantOpen || xrcd.Pointer() != unsafe.Pointer(p.xrcd) {
		t.Errorf("open_xrcd(%#x, %+v) gave %p, want open_xrcd(%#x, %+v) giving %p", p.openCtx, p.openAttr, xrcd.Pointer(), ctx, wantOpen, p.xrcd)
	}

	pd := PDFromPointer(cMemory(t, 64))
	sendCQ, recvCQ := fakeCQ(t), fakeCQ(t)
	sattr := SRQInitAttr{Type: SRQTypeXRC, Attr: SRQAttr{MaxWR: 8, MaxSGE: 1, Limit: 2}, XRCD: xrcd, CQ: recvCQ}
	srq, err := pd.CreateSRQ(&sattr)
	if err != nil {
		t.Fatalf("CreateSRQ(XRC) error: %v", err)
	}
	t.Cleanup(func() { srqs.Delete(caddr(p.srq)) })
	wantSRQ := rdmaabi.IbvSrqInitAttrEx{
		Attr:      rdmaabi.IbvSrqAttr{Max_wr: 8, Max_sge: 1, Srq_limit: 2},
		Comp_mask: 1<<0 | 1<<1 | 1<<2 | 1<<3, // TYPE, PD, XRCD and CQ
		Srq_type:  uint32(SRQTypeXRC),
		Pd:        uint64(uintptr(pd.Pointer())),
		Xrcd:      uint64(caddr(p.xrcd)),
		Cq:        uint64(uintptr(recvCQ.Pointer())),
	}
	if p.srqCtx != ctx || p.srqAttr != wantSRQ || srq.Pointer() != unsafe.Pointer(p.srq) {
		t.Errorf("create_srq_ex(%#x, %+v) gave %p, want create_srq_ex(%#x, %+v) giving %p", p.srqCtx, p.srqAttr, srq.Pointer(), ctx, wantSRQ, p.srq)
	}
	if sattr.Attr != (SRQAttr{MaxWR: 15, MaxSGE: 1, Limit: 2}) {
		t.Errorf("CreateSRQ() granted %+v, want the provider's 15 receives", sattr.Attr)
	}
	if lookupSRQ(caddr(p.srq)) != srq {
		t.Error("the events do not know the SRQ")
	}
	if n, err := srq.Num(); err != nil || n != 0xabcdef || p.numSRQ != caddr(p.srq) {
		t.Errorf("Num() = %#x, %v from get_srq_num(%#x, ...), want 0xabcdef from get_srq_num(%p, ...)", n, err, p.numSRQ, p.srq)
	}

	qattr := QPInitAttr{
		SendCQ:    sendCQ,
		RecvCQ:    recvCQ,
		SRQ:       srq,
		XRCD:      xrcd,
		Type:      QPTypeXRCRecv,
		Cap:       QPCap{MaxSendWR: 1, MaxRecvWR: 2, MaxSendSGE: 3, MaxRecvSGE: 4, MaxInlineData: 5},
		SignalAll: true,
	}
	qp, err := pd.CreateQP(&qattr)
	if err != nil {
		t.Fatalf("CreateQP(XRC_RECV) error: %v", err)
	}
	t.Cleanup(func() { qps.Delete(caddr(p.qp)) })
	wantQP := rdmaabi.IbvQpInitAttrEx{
		Send_cq:    uint64(uintptr(sendCQ.Pointer())),
		Recv_cq:    uint64(uintptr(recvCQ.Pointer())),
		Srq:        uint64(caddr(p.srq)),
		Cap:        rdmaabi.IbvQpCap{Max_send_wr: 1, Max_recv_wr: 2, Max_send_sge: 3, Max_recv_sge: 4, Max_inline_data: 5},
		Qp_type:    uint32(QPTypeXRCRecv),
		Sq_sig_all: 1,
		Comp_mask:  1<<0 | 1<<1, // PD and XRCD
		Pd:         uint64(uintptr(pd.Pointer())),
		Xrcd:       uint64(caddr(p.xrcd)),
	}
	if p.qpCtx != ctx || p.qpAttr != wantQP || qp.Pointer() != unsafe.Pointer(p.qp) || qp.Type() != QPTypeXRCRecv {
		t.Errorf("create_qp_ex(%#x, %+v) gave %p of type %v, want create_qp_ex(%#x, %+v) giving %p", p.qpCtx, p.qpAttr, qp.Pointer(), qp.Type(), ctx, wantQP, p.qp)
	}
	if qattr.Cap.MaxInlineData != 64 {
		t.Errorf("CreateQP() granted %+v, want the provider's 64 bytes of inline data", qattr.Cap)
	}
	if lookupQP(caddr(p.qp)) != qp {
		t.Error("the events do not know the queue pair")
	}

	for range 2 {
		if err := xrcd.Close(); err != nil {
			t.Errorf("Close() error: %v", err)
		}
	}
	if want := []uintptr{caddr(p.xrcd)}; !slices.Equal(p.closed, want) || xrcd.Pointer() != nil {
		t.Errorf("two Close() called close_xrcd with %#x, leaving %p; want one call with %#x", p.closed, xrcd.Pointer(), want)
	}
}

// Only an XRC receive queue pair belongs to a domain and goes to
// create_qp_ex: a domain given for another type is refused before the
// library or the provider sees it, as older libraries take its handle for
// the PD, and so is an XRC receive queue pair without one.
func TestOnlyXRCReceiveQueuePairsTakeADomain(t *testing.T) {
	useLibrary(t, missingLibrary)
	p := newXRCProvider(t)
	xrcd := XRCDFromPointer(unsafe.Pointer(p.xrcd))
	pd := PDFromPointer(cMemory(t, 64))
	var opErr *OpError
	for _, typ := range []QPType{QPTypeRC, QPTypeUC, QPTypeUD, QPTypeXRCSend} {
		_, err := pd.CreateQP(&QPInitAttr{SendCQ: fakeCQ(t), RecvCQ: fakeCQ(t), XRCD: xrcd, Type: typ, Cap: QPCap{MaxSendWR: 1, MaxSendSGE: 1}})
		if !errors.As(err, &opErr) || opErr.Op != "ibv_create_qp" || !errors.Is(err, syscall.EINVAL) {
			t.Errorf("CreateQP(%v) in an XRC domain: error = %v, want an *OpError of ibv_create_qp matching EINVAL", typ, err)
		}
	}
	_, err := pd.CreateQP(&QPInitAttr{Type: QPTypeXRCRecv})
	if !errors.As(err, &opErr) || opErr.Op != "ibv_create_qp_ex" || !errors.Is(err, syscall.EINVAL) {
		t.Errorf("CreateQP(XRC_RECV) without a domain: error = %v, want an *OpError of ibv_create_qp_ex matching EINVAL", err)
	}
	if p.qpCalls != 0 {
		t.Errorf("create_qp_ex called %d times, want none", p.qpCalls)
	}
}
