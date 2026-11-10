//go:build linux && !android && (amd64 || arm64 || loong64 || ppc64le || riscv64 || s390x)

package verbs

import (
	"encoding/binary"
	"sync"
	"unsafe"

	"github.com/wzshiming/rdma/internal/dynlib"
	"github.com/wzshiming/rdma/internal/rdmaabi"
)

// qps maps struct ibv_qp pointers to the QPs this package created, so that
// asynchronous events can name their Go object.
var qps sync.Map

// QP is a queue pair (struct ibv_qp).
type QP struct {
	qp       *rdmaabi.IbvQp
	owned    bool
	postSend dynlib.Func // post_send of the device context
	postRecv dynlib.Func // post_recv of the device context
	send     postBuf[rdmaabi.IbvSendWr]
	recv     postBuf[rdmaabi.IbvRecvWr]
}

// postBuf is the memory the work requests of one queue are built in: the
// C-layout chain, its SGE array and the slot ibv_post_* store the rejected
// request in. It is kept from post to post, so a steady state allocates
// nothing, and its mutex serialises the posts of the queue, as the provider
// does on its own.
type postBuf[WR rdmaabi.IbvSendWr | rdmaabi.IbvRecvWr] struct {
	mu   sync.Mutex
	wrs  []WR
	sges []rdmaabi.IbvSge
	bad  uint64
}

// reserve returns zeroed room for nwr work requests and nsge SGEs; the
// caller holds mu.
func (b *postBuf[WR]) reserve(nwr, nsge int) ([]WR, []rdmaabi.IbvSge) {
	if cap(b.wrs) < nwr {
		b.wrs = make([]WR, nwr)
	}
	if cap(b.sges) < nsge {
		b.sges = make([]rdmaabi.IbvSge, nsge)
	}
	wrs, sges := b.wrs[:nwr], b.sges[:nsge]
	clear(wrs)
	return wrs, sges
}

// newQP wraps qp, taking the inline verbs from its context's ops table.
func newQP(qp *rdmaabi.IbvQp, owned bool) *QP {
	q := &QP{qp: qp, owned: owned}
	if qp != nil {
		ops := contextOps(qp.Context)
		q.postSend = dynlib.FuncAt(uintptr(ops.Post_send))
		q.postRecv = dynlib.FuncAt(uintptr(ops.Post_recv))
	}
	return q
}

// CreateQP creates a queue pair and stores the granted capacities in attr.Cap.
func (p *PD) CreateQP(attr *QPInitAttr) (*QP, error) {
	f, err := load("ibv_create_qp")
	if err != nil {
		return nil, err
	}
	var ia rdmaabi.IbvQpInitAttr
	if attr.SendCQ != nil {
		ia.Send_cq = uint64(caddr(attr.SendCQ.cq))
	}
	if attr.RecvCQ != nil {
		ia.Recv_cq = uint64(caddr(attr.RecvCQ.cq))
	}
	ia.Cap = capToC(attr.Cap)
	ia.Qp_type = uint32(attr.Type)
	if attr.SignalAll {
		ia.Sq_sig_all = 1
	}
	qp, errno := f.createQP.Ptr(uintptr(p.pd), uintptr(unsafe.Pointer(&ia)), 0, 0, 0, 0)
	if qp == 0 {
		return nil, opError("ibv_create_qp", errno)
	}
	attr.Cap = capFromC(ia.Cap)
	q := newQP(dynlib.Pointer[rdmaabi.IbvQp](qp), true)
	qps.Store(qp, q)
	return q, nil
}

// lookupQP returns the QP created for the struct ibv_qp at addr, or a
// non-owning wrapper.
func lookupQP(addr uintptr) *QP {
	if v, ok := qps.Load(addr); ok {
		return v.(*QP)
	}
	return newQP(dynlib.Pointer[rdmaabi.IbvQp](addr), false)
}

// QPFromPointer wraps a struct ibv_qp pointer owned by someone else, such as
// the queue pair of an rdma_cm id; Close does not destroy it.
func QPFromPointer(p unsafe.Pointer) *QP { return newQP((*rdmaabi.IbvQp)(p), false) }

// Pointer returns the underlying struct ibv_qp pointer.
func (q *QP) Pointer() unsafe.Pointer { return unsafe.Pointer(q.qp) }

// Num returns the queue pair number.
func (q *QP) Num() uint32 { return q.qp.Qp_num }

// Type returns the transport service type.
func (q *QP) Type() QPType { return QPType(q.qp.Qp_type) }

// Modify applies the attr fields selected by mask, as ibv_modify_qp does.
func (q *QP) Modify(attr *QPAttr, mask QPAttrMask) error {
	f, err := load("ibv_modify_qp")
	if err != nil {
		return err
	}
	ca := attrToC(attr)
	if rc, _ := f.modifyQP.Int(caddr(q.qp), uintptr(unsafe.Pointer(&ca)), cint(int32(mask)), 0, 0, 0); rc != 0 {
		return opErrorRC("ibv_modify_qp", rc)
	}
	return nil
}

// Query returns the attributes selected by mask; Cap is always filled.
func (q *QP) Query(mask QPAttrMask) (*QPAttr, error) {
	f, err := load("ibv_query_qp")
	if err != nil {
		return nil, err
	}
	var ca rdmaabi.IbvQpAttr
	var ia rdmaabi.IbvQpInitAttr
	if rc, _ := f.queryQP.Int(caddr(q.qp), uintptr(unsafe.Pointer(&ca)), cint(int32(mask)), uintptr(unsafe.Pointer(&ia)), 0, 0); rc != 0 {
		return nil, opErrorRC("ibv_query_qp", rc)
	}
	attr := attrFromC(&ca)
	attr.Cap = capFromC(ia.Cap)
	return attr, nil
}

// PostSend posts send work requests as one chain. The chain and its SGE
// array live in the queue pair's own buffer for the duration of the call;
// afterwards the library keeps no reference to them.
func (q *QP) PostSend(wrs ...SendWR) error {
	if len(wrs) == 0 {
		return nil
	}
	nsge := 0
	for i := range wrs {
		nsge += len(wrs[i].SGE)
	}
	q.send.mu.Lock()
	defer q.send.mu.Unlock()
	cwrs, sges := q.send.reserve(len(wrs), nsge)
	for i := range wrs {
		w, cw := &wrs[i], &cwrs[i]
		sendWRToC(cw, w)
		if i+1 < len(wrs) {
			cw.Next = uint64(uintptr(unsafe.Pointer(&cwrs[i+1])))
		}
		if len(w.SGE) > 0 {
			cw.Sg_list = uint64(uintptr(unsafe.Pointer(&sges[0])))
			cw.Num_sge = int32(len(w.SGE))
			sges = fillSGEs(sges, w.SGE)
		}
	}
	// &q.send.bad keeps q, and with it the chain, alive for the call.
	rc, _ := q.postSend.Int(caddr(q.qp), uintptr(unsafe.Pointer(&cwrs[0])), uintptr(unsafe.Pointer(&q.send.bad)), 0, 0, 0)
	if rc != 0 {
		return opErrorRC("ibv_post_send", rc)
	}
	return nil
}

// sendWRToC fills the fields of cw that come from w; the chain and the SGE
// list are the caller's. Immediate data goes out in network byte order.
func sendWRToC(cw *rdmaabi.IbvSendWr, w *SendWR) {
	cw.Wr_id = w.ID
	cw.Opcode = uint32(w.Op)
	cw.Send_flags = uint32(w.Flags)
	binary.BigEndian.PutUint32(cw.Imm_data[:], w.Imm)
	cw.Wr.Rdma.Remote_addr = w.RemoteAddr
	cw.Wr.Rdma.Rkey = w.RKey
}

// PostRecv posts receive work requests as one chain, built in the queue
// pair's buffer like PostSend does.
func (q *QP) PostRecv(wrs ...RecvWR) error {
	if len(wrs) == 0 {
		return nil
	}
	nsge := 0
	for i := range wrs {
		nsge += len(wrs[i].SGE)
	}
	q.recv.mu.Lock()
	defer q.recv.mu.Unlock()
	cwrs, sges := q.recv.reserve(len(wrs), nsge)
	for i := range wrs {
		w, cw := &wrs[i], &cwrs[i]
		cw.Wr_id = w.ID
		if i+1 < len(wrs) {
			cw.Next = uint64(uintptr(unsafe.Pointer(&cwrs[i+1])))
		}
		if len(w.SGE) > 0 {
			cw.Sg_list = uint64(uintptr(unsafe.Pointer(&sges[0])))
			cw.Num_sge = int32(len(w.SGE))
			sges = fillSGEs(sges, w.SGE)
		}
	}
	rc, _ := q.postRecv.Int(caddr(q.qp), uintptr(unsafe.Pointer(&cwrs[0])), uintptr(unsafe.Pointer(&q.recv.bad)), 0, 0, 0)
	if rc != 0 {
		return opErrorRC("ibv_post_recv", rc)
	}
	return nil
}

// fillSGEs copies src to the front of dst and returns the rest of dst.
func fillSGEs(dst []rdmaabi.IbvSge, src []SGE) []rdmaabi.IbvSge {
	for i, s := range src {
		dst[i] = rdmaabi.IbvSge{Addr: s.Addr, Length: s.Length, Lkey: s.LKey}
	}
	return dst[len(src):]
}

// Close destroys the queue pair. A second Close is a no-op.
func (q *QP) Close() error {
	qp := q.qp
	if qp == nil {
		return nil
	}
	if q.owned {
		f, err := load("ibv_destroy_qp")
		if err != nil {
			return err
		}
		if rc, _ := f.destroyQP.Int(caddr(qp), 0, 0, 0, 0, 0); rc != 0 {
			return opErrorRC("ibv_destroy_qp", rc)
		}
		qps.Delete(caddr(qp))
	}
	q.qp = nil
	return nil
}

func capToC(c QPCap) rdmaabi.IbvQpCap {
	return rdmaabi.IbvQpCap{
		Max_send_wr:     c.MaxSendWR,
		Max_recv_wr:     c.MaxRecvWR,
		Max_send_sge:    c.MaxSendSGE,
		Max_recv_sge:    c.MaxRecvSGE,
		Max_inline_data: c.MaxInlineData,
	}
}

func capFromC(c rdmaabi.IbvQpCap) QPCap {
	return QPCap{
		MaxSendWR:     c.Max_send_wr,
		MaxRecvWR:     c.Max_recv_wr,
		MaxSendSGE:    c.Max_send_sge,
		MaxRecvSGE:    c.Max_recv_sge,
		MaxInlineData: c.Max_inline_data,
	}
}

func ahToC(a AHAttr) rdmaabi.IbvAhAttr {
	ca := rdmaabi.IbvAhAttr{
		Grh: rdmaabi.IbvGlobalRoute{
			Dgid:          rdmaabi.IbvGid(a.GRH.DGID),
			Flow_label:    a.GRH.FlowLabel,
			Sgid_index:    a.GRH.SGIDIndex,
			Hop_limit:     a.GRH.HopLimit,
			Traffic_class: a.GRH.TrafficClass,
		},
		Dlid:          a.DLID,
		Sl:            a.SL,
		Src_path_bits: a.SrcPathBits,
		Static_rate:   a.StaticRate,
		Port_num:      a.Port,
	}
	if a.IsGlobal {
		ca.Is_global = 1
	}
	return ca
}

func ahFromC(ca *rdmaabi.IbvAhAttr) AHAttr {
	return AHAttr{
		GRH: GlobalRoute{
			DGID:         GID(ca.Grh.Dgid),
			FlowLabel:    ca.Grh.Flow_label,
			SGIDIndex:    ca.Grh.Sgid_index,
			HopLimit:     ca.Grh.Hop_limit,
			TrafficClass: ca.Grh.Traffic_class,
		},
		DLID:        ca.Dlid,
		SL:          ca.Sl,
		SrcPathBits: ca.Src_path_bits,
		StaticRate:  ca.Static_rate,
		IsGlobal:    ca.Is_global != 0,
		Port:        ca.Port_num,
	}
}

func attrToC(a *QPAttr) rdmaabi.IbvQpAttr {
	ca := rdmaabi.IbvQpAttr{
		Qp_state:           uint32(a.State),
		Cur_qp_state:       uint32(a.CurState),
		Path_mtu:           uint32(a.PathMTU),
		Path_mig_state:     uint32(a.PathMigState),
		Qkey:               a.QKey,
		Rq_psn:             a.RQPSN,
		Sq_psn:             a.SQPSN,
		Dest_qp_num:        a.DestQPNum,
		Qp_access_flags:    uint32(a.AccessFlags),
		Cap:                capToC(a.Cap),
		Ah_attr:            ahToC(a.AH),
		Alt_ah_attr:        ahToC(a.AltAH),
		Pkey_index:         a.PkeyIndex,
		Alt_pkey_index:     a.AltPkeyIndex,
		Max_rd_atomic:      a.MaxRDAtomic,
		Max_dest_rd_atomic: a.MaxDestRDAtomic,
		Min_rnr_timer:      a.MinRNRTimer,
		Port_num:           a.Port,
		Timeout:            a.Timeout,
		Retry_cnt:          a.RetryCnt,
		Rnr_retry:          a.RNRRetry,
		Alt_port_num:       a.AltPort,
		Alt_timeout:        a.AltTimeout,
		Rate_limit:         a.RateLimit,
	}
	if a.EnSQDAsyncNotify {
		ca.En_sqd_async_notify = 1
	}
	if a.SQDraining {
		ca.Sq_draining = 1
	}
	return ca
}

func attrFromC(ca *rdmaabi.IbvQpAttr) *QPAttr {
	return &QPAttr{
		State:            QPState(ca.Qp_state),
		CurState:         QPState(ca.Cur_qp_state),
		PathMTU:          MTU(ca.Path_mtu),
		PathMigState:     MigState(ca.Path_mig_state),
		QKey:             ca.Qkey,
		RQPSN:            ca.Rq_psn,
		SQPSN:            ca.Sq_psn,
		DestQPNum:        ca.Dest_qp_num,
		AccessFlags:      AccessFlags(ca.Qp_access_flags),
		Cap:              capFromC(ca.Cap),
		AH:               ahFromC(&ca.Ah_attr),
		AltAH:            ahFromC(&ca.Alt_ah_attr),
		PkeyIndex:        ca.Pkey_index,
		AltPkeyIndex:     ca.Alt_pkey_index,
		EnSQDAsyncNotify: ca.En_sqd_async_notify != 0,
		SQDraining:       ca.Sq_draining != 0,
		MaxRDAtomic:      ca.Max_rd_atomic,
		MaxDestRDAtomic:  ca.Max_dest_rd_atomic,
		MinRNRTimer:      ca.Min_rnr_timer,
		Port:             ca.Port_num,
		Timeout:          ca.Timeout,
		RetryCnt:         ca.Retry_cnt,
		RNRRetry:         ca.Rnr_retry,
		AltPort:          ca.Alt_port_num,
		AltTimeout:       ca.Alt_timeout,
		RateLimit:        ca.Rate_limit,
	}
}
