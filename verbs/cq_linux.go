//go:build linux && cgo

package verbs

/*
#include <stddef.h>
#include <stdlib.h>
#include <infiniband/verbs.h>

// imm_data lives in an anonymous union, which cgo cannot name.
enum { VERBS_WC_IMM_OFFSET = offsetof(struct ibv_wc, imm_data) };
*/
import "C"

import (
	"os"
	"sync"
	"syscall"
	"unsafe"
)

// wcBatch is the size of the per-CQ scratch array ibv_poll_cq fills.
const wcBatch = 32

// cqs maps struct ibv_cq pointers to the CQs this package created, so that
// events can name their Go object.
var cqs sync.Map

// CompChannel is a completion event channel (struct ibv_comp_channel).
type CompChannel struct {
	ch     *C.struct_ibv_comp_channel
	owned  bool
	waiter fdWaiter
}

// CreateCompChannel creates a completion event channel.
func (c *Context) CreateCompChannel() (*CompChannel, error) {
	ch, err := C.ibv_create_comp_channel(c.ctx)
	if ch == nil {
		return nil, opError("ibv_create_comp_channel", err)
	}
	return &CompChannel{ch: ch, owned: true}, nil
}

// CompChannelFromPointer wraps a struct ibv_comp_channel pointer owned by
// someone else; Close does not destroy it.
func CompChannelFromPointer(p unsafe.Pointer) *CompChannel {
	return &CompChannel{ch: (*C.struct_ibv_comp_channel)(p)}
}

// Pointer returns the underlying struct ibv_comp_channel pointer.
func (c *CompChannel) Pointer() unsafe.Pointer { return unsafe.Pointer(c.ch) }

// Close destroys the channel, which fails with EBUSY while a CQ still uses it,
// and wakes a pending Wait with an error. A second Close is a no-op.
func (c *CompChannel) Close() error {
	ch := c.ch
	if ch == nil {
		return nil
	}
	c.waiter.close()
	if c.owned {
		if rc := C.ibv_destroy_comp_channel(ch); rc != 0 {
			return opErrorRC("ibv_destroy_comp_channel", rc)
		}
	}
	c.ch = nil
	return nil
}

// CQ is a completion queue (struct ibv_cq).
type CQ struct {
	cq    *C.struct_ibv_cq
	owned bool
	mu    sync.Mutex
	wcs   *C.struct_ibv_wc // scratch array of wcBatch entries for Poll
}

// CreateCQ creates a completion queue with at least cqe entries, delivering
// its events to ch when ch is not nil.
func (c *Context) CreateCQ(cqe int, ch *CompChannel) (*CQ, error) {
	var cch *C.struct_ibv_comp_channel
	if ch != nil {
		cch = ch.ch
	}
	cq, err := C.ibv_create_cq(c.ctx, C.int(cqe), nil, cch, 0)
	if cq == nil {
		return nil, opError("ibv_create_cq", err)
	}
	q := &CQ{cq: cq, owned: true}
	cqs.Store(uintptr(unsafe.Pointer(cq)), q)
	return q, nil
}

// lookupCQ returns the CQ created for cq, or a non-owning wrapper.
func lookupCQ(cq *C.struct_ibv_cq) *CQ {
	if v, ok := cqs.Load(uintptr(unsafe.Pointer(cq))); ok {
		return v.(*CQ)
	}
	return &CQ{cq: cq}
}

// CQFromPointer wraps a struct ibv_cq pointer owned by someone else; Close
// does not destroy it.
func CQFromPointer(p unsafe.Pointer) *CQ { return &CQ{cq: (*C.struct_ibv_cq)(p)} }

// Pointer returns the underlying struct ibv_cq pointer.
func (q *CQ) Pointer() unsafe.Pointer { return unsafe.Pointer(q.cq) }

// Poll removes up to len(wcs) completions from the queue and returns how many
// it stored in wcs.
func (q *CQ) Poll(wcs []WC) (int, error) {
	if len(wcs) == 0 {
		return 0, nil
	}
	q.mu.Lock()
	defer q.mu.Unlock()
	if q.cq == nil {
		return 0, &OpError{Op: "ibv_poll_cq", Err: os.ErrClosed}
	}
	if q.wcs == nil {
		q.wcs = (*C.struct_ibv_wc)(C.malloc(wcBatch * C.size_t(unsafe.Sizeof(C.struct_ibv_wc{}))))
		if q.wcs == nil {
			return 0, &OpError{Op: "ibv_poll_cq", Err: syscall.ENOMEM}
		}
	}
	buf := unsafe.Slice(q.wcs, wcBatch)
	total := 0
	for total < len(wcs) {
		n := min(len(wcs)-total, wcBatch)
		rc, err := C.ibv_poll_cq(q.cq, C.int(n), q.wcs)
		if rc < 0 {
			return total, opError("ibv_poll_cq", err)
		}
		for i := range int(rc) {
			wcs[total+i] = wcFromC(&buf[i])
		}
		total += int(rc)
		if int(rc) < n {
			break
		}
	}
	return total, nil
}

func wcFromC(w *C.struct_ibv_wc) WC {
	return WC{
		ID:           uint64(w.wr_id),
		Status:       WCStatus(w.status),
		Opcode:       WCOpcode(w.opcode),
		VendorErr:    uint32(w.vendor_err),
		Len:          uint32(w.byte_len),
		Imm:          *(*uint32)(unsafe.Add(unsafe.Pointer(w), C.VERBS_WC_IMM_OFFSET)),
		QPNum:        uint32(w.qp_num),
		SrcQP:        uint32(w.src_qp),
		Flags:        WCFlags(w.wc_flags),
		PkeyIndex:    uint16(w.pkey_index),
		SLID:         uint16(w.slid),
		SL:           uint8(w.sl),
		DLIDPathBits: uint8(w.dlid_path_bits),
	}
}

// RequestNotify arms the completion channel for the next completion, or for
// the next solicited completion only.
func (q *CQ) RequestNotify(solicitedOnly bool) error {
	var s C.int
	if solicitedOnly {
		s = 1
	}
	if rc := C.ibv_req_notify_cq(q.cq, s); rc != 0 {
		return opErrorRC("ibv_req_notify_cq", rc)
	}
	return nil
}

// Close destroys the completion queue. A second Close is a no-op.
func (q *CQ) Close() error {
	q.mu.Lock()
	defer q.mu.Unlock()
	cq := q.cq
	if cq == nil {
		return nil
	}
	if q.owned {
		if rc := C.ibv_destroy_cq(cq); rc != 0 {
			return opErrorRC("ibv_destroy_cq", rc)
		}
		cqs.Delete(uintptr(unsafe.Pointer(cq)))
	}
	q.cq = nil
	if q.wcs != nil {
		C.free(unsafe.Pointer(q.wcs))
		q.wcs = nil
	}
	return nil
}

// wcStatusStr returns the libibverbs text of a status; it backs the test that
// keeps wcStatusNames in step with ibv_wc_status_str.
func wcStatusStr(s WCStatus) string {
	return C.GoString(C.ibv_wc_status_str(uint32(s)))
}
