//go:build linux && !android && (amd64 || arm64 || loong64 || ppc64le || riscv64 || s390x)

package verbs

import (
	"encoding/binary"
	"os"
	"sync"
	"sync/atomic"
	"unsafe"

	"github.com/wzshiming/rdma/internal/dynlib"
	"github.com/wzshiming/rdma/internal/rdmaabi"
)

// wcBatch is the size of the per-CQ scratch array poll_cq fills.
const wcBatch = 32

// cqs maps struct ibv_cq pointers to the CQs this package created, so that
// events can name their Go object.
var cqs sync.Map

// CompChannel is a completion event channel (struct ibv_comp_channel).
type CompChannel struct {
	ch     *rdmaabi.IbvCompChannel
	owned  bool
	waiter fdWaiter
	next   func() (bool, error)           // nextEvent, made once for every wait
	event  struct{ cq, cqContext uint64 } // struct ibv_cq *, void *, as nextEvent took them
}

// newCompChannel wraps ch, with the try of Wait made once.
func newCompChannel(ch *rdmaabi.IbvCompChannel, owned bool) *CompChannel {
	c := &CompChannel{ch: ch, owned: owned}
	c.next = c.nextEvent
	return c
}

// CreateCompChannel creates a completion event channel.
func (c *Context) CreateCompChannel() (*CompChannel, error) {
	f, err := load("ibv_create_comp_channel")
	if err != nil {
		return nil, err
	}
	ch, errno := f.createCompChannel.Ptr(caddr(c.ctx), 0, 0, 0, 0, 0)
	if ch == 0 {
		return nil, opError("ibv_create_comp_channel", errno)
	}
	return newCompChannel(dynlib.Pointer[rdmaabi.IbvCompChannel](ch), true), nil
}

// CompChannelFromPointer wraps a struct ibv_comp_channel pointer owned by
// someone else; Close does not destroy it.
func CompChannelFromPointer(p unsafe.Pointer) *CompChannel {
	return newCompChannel((*rdmaabi.IbvCompChannel)(p), false)
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
		f, err := load("ibv_destroy_comp_channel")
		if err != nil {
			return err
		}
		if rc, _ := f.destroyCompChannel.Int(caddr(ch), 0, 0, 0, 0, 0); rc != 0 {
			return opErrorRC("ibv_destroy_comp_channel", rc)
		}
	}
	c.ch = nil
	return nil
}

// CQ is a completion queue (struct ibv_cq).
type CQ struct {
	cq        *rdmaabi.IbvCq
	owned     bool
	pollCQ    dynlib.Func // poll_cq of the device context
	reqNotify dynlib.Func // req_notify_cq of the device context
	mu        sync.Mutex
	wcs       []rdmaabi.IbvWc // scratch array of wcBatch entries for Poll
	events    atomic.Uint32   // taken by Wait, not acknowledged yet
	closing   atomic.Bool     // set by Close before it acknowledges them
}

// newCQ wraps cq, taking the inline verbs from its context's ops table.
func newCQ(cq *rdmaabi.IbvCq, owned bool) *CQ {
	q := &CQ{cq: cq, owned: owned}
	if cq != nil {
		ops := contextOps(cq.Context)
		q.pollCQ = dynlib.FuncAt(uintptr(ops.Poll_cq))
		q.reqNotify = dynlib.FuncAt(uintptr(ops.Req_notify_cq))
	}
	return q
}

// CreateCQ creates a completion queue with at least cqe entries, delivering
// its events to ch when ch is not nil.
func (c *Context) CreateCQ(cqe int, ch *CompChannel) (*CQ, error) {
	f, err := load("ibv_create_cq")
	if err != nil {
		return nil, err
	}
	var cch uintptr
	if ch != nil {
		cch = caddr(ch.ch)
	}
	cq, errno := f.createCQ.Ptr(caddr(c.ctx), cint(int32(cqe)), 0, cch, 0, 0)
	if cq == 0 {
		return nil, opError("ibv_create_cq", errno)
	}
	q := newCQ(dynlib.Pointer[rdmaabi.IbvCq](cq), true)
	cqs.Store(cq, q)
	return q, nil
}

// lookupCQ returns the CQ created for the struct ibv_cq at addr, or a
// non-owning wrapper.
func lookupCQ(addr uintptr) *CQ {
	if v, ok := cqs.Load(addr); ok {
		return v.(*CQ)
	}
	return newCQ(dynlib.Pointer[rdmaabi.IbvCq](addr), false)
}

// ackBatch is how many events of a CQ of CreateCQ Wait acknowledges in one
// call.
const ackBatch = 64

// tookEvent counts an event Wait took for q and returns how many events of q
// to acknowledge now: a whole batch at a time for a CQ of CreateCQ, whose
// Close acknowledges the rest; each event at once for a CQ made elsewhere,
// which its owner may destroy any time, and once Close is under way.
func (q *CQ) tookEvent() uint32 {
	if !q.owned {
		return 1
	}
	if q.events.Add(1) < ackBatch && !q.closing.Load() {
		return 0
	}
	return q.events.Swap(0)
}

// closeEvents has Wait acknowledge the events of q at once from now on and
// returns how many it took and left unacknowledged.
func (q *CQ) closeEvents() uint32 {
	q.closing.Store(true)
	return q.events.Swap(0)
}

// CQFromPointer wraps a struct ibv_cq pointer owned by someone else; Close
// does not destroy it.
func CQFromPointer(p unsafe.Pointer) *CQ { return newCQ((*rdmaabi.IbvCq)(p), false) }

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
		q.wcs = make([]rdmaabi.IbvWc, wcBatch)
	}
	total := 0
	for total < len(wcs) {
		n := min(len(wcs)-total, wcBatch)
		rc, errno := q.pollCQ.Int(caddr(q.cq), cint(int32(n)), uintptr(unsafe.Pointer(&q.wcs[0])), 0, 0, 0)
		if rc < 0 {
			return total, opError("ibv_poll_cq", errno)
		}
		for i := range int(rc) {
			wcs[total+i] = wcFromC(&q.wcs[i])
		}
		total += int(rc)
		if int(rc) < n {
			break
		}
	}
	return total, nil
}

func wcFromC(w *rdmaabi.IbvWc) WC {
	return WC{
		ID:           w.Wr_id,
		Status:       WCStatus(w.Status),
		Opcode:       WCOpcode(w.Opcode),
		VendorErr:    w.Vendor_err,
		Len:          w.Byte_len,
		Imm:          binary.BigEndian.Uint32(w.Imm_data[:]),
		QPNum:        w.Qp_num,
		SrcQP:        w.Src_qp,
		Flags:        WCFlags(w.Wc_flags),
		PkeyIndex:    w.Pkey_index,
		SLID:         w.Slid,
		SL:           w.Sl,
		DLIDPathBits: w.Dlid_path_bits,
	}
}

// RequestNotify arms the completion channel for the next completion, or for
// the next solicited completion only.
func (q *CQ) RequestNotify(solicitedOnly bool) error {
	var s int32
	if solicitedOnly {
		s = 1
	}
	if rc, _ := q.reqNotify.Int(caddr(q.cq), cint(s), 0, 0, 0, 0); rc != 0 {
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
		f, err := load("ibv_destroy_cq")
		if err != nil {
			return err
		}
		// ibv_destroy_cq waits until every event of the CQ is acknowledged.
		if n := q.closeEvents(); n > 0 {
			f.ackCQEvents.Int(caddr(cq), uintptr(n), 0, 0, 0, 0)
		}
		if rc, _ := f.destroyCQ.Int(caddr(cq), 0, 0, 0, 0, 0); rc != 0 {
			return opErrorRC("ibv_destroy_cq", rc)
		}
		cqs.Delete(caddr(cq))
	}
	q.cq = nil
	q.wcs = nil
	return nil
}

// wcStatusStr returns the libibverbs text of a status; it backs the test that
// keeps wcStatusNames in step with ibv_wc_status_str.
func wcStatusStr(s WCStatus) (string, error) {
	f, err := load("ibv_wc_status_str")
	if err != nil {
		return "", err
	}
	str, _ := f.wcStatusStr.Ptr(cint(int32(s)), 0, 0, 0, 0, 0)
	return goString(str), nil
}
