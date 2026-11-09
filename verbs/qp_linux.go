//go:build linux && cgo

package verbs

/*
#include <infiniband/verbs.h>
*/
import "C"

import (
	"sync"
	"syscall"
	"unsafe"
)

// qps maps struct ibv_qp pointers to the QPs this package created, so that
// asynchronous events can name their Go object.
var qps sync.Map

// QP is a queue pair (struct ibv_qp).
type QP struct {
	qp    *C.struct_ibv_qp
	owned bool
}

// CreateQP creates a queue pair and stores the granted capacities in attr.Cap.
func (p *PD) CreateQP(attr *QPInitAttr) (*QP, error) {
	var ia C.struct_ibv_qp_init_attr
	if attr.SendCQ != nil {
		ia.send_cq = attr.SendCQ.cq
	}
	if attr.RecvCQ != nil {
		ia.recv_cq = attr.RecvCQ.cq
	}
	ia.cap = capToC(attr.Cap)
	ia.qp_type = uint32(attr.Type)
	if attr.SignalAll {
		ia.sq_sig_all = 1
	}
	qp, err := C.ibv_create_qp(p.pd, &ia)
	if qp == nil {
		return nil, opError("ibv_create_qp", err)
	}
	attr.Cap = capFromC(ia.cap)
	q := &QP{qp: qp, owned: true}
	qps.Store(uintptr(unsafe.Pointer(qp)), q)
	return q, nil
}

// lookupQP returns the QP created for qp, or a non-owning wrapper.
func lookupQP(qp *C.struct_ibv_qp) *QP {
	if v, ok := qps.Load(uintptr(unsafe.Pointer(qp))); ok {
		return v.(*QP)
	}
	return &QP{qp: qp}
}

// QPFromPointer wraps a struct ibv_qp pointer owned by someone else, such as
// the queue pair of an rdma_cm id; Close does not destroy it.
func QPFromPointer(p unsafe.Pointer) *QP { return &QP{qp: (*C.struct_ibv_qp)(p)} }

// Pointer returns the underlying struct ibv_qp pointer.
func (q *QP) Pointer() unsafe.Pointer { return unsafe.Pointer(q.qp) }

// Num returns the queue pair number.
func (q *QP) Num() uint32 { return uint32(q.qp.qp_num) }

// Type returns the transport service type.
func (q *QP) Type() QPType { return QPType(q.qp.qp_type) }

// Modify applies the attr fields selected by mask, as ibv_modify_qp does.
func (q *QP) Modify(attr *QPAttr, mask QPAttrMask) error {
	ca := attrToC(attr)
	if rc := C.ibv_modify_qp(q.qp, &ca, C.int(mask)); rc != 0 {
		return opErrorRC("ibv_modify_qp", rc)
	}
	return nil
}

// Query returns the attributes selected by mask; Cap is always filled.
func (q *QP) Query(mask QPAttrMask) (*QPAttr, error) {
	var ca C.struct_ibv_qp_attr
	var ia C.struct_ibv_qp_init_attr
	if rc := C.ibv_query_qp(q.qp, &ca, C.int(mask), &ia); rc != 0 {
		return nil, opErrorRC("ibv_query_qp", rc)
	}
	attr := attrFromC(&ca)
	attr.Cap = capFromC(ia.cap)
	return attr, nil
}

// PostSend posts send work requests as one chain; after it returns the C side
// keeps no reference to Go memory.
func (q *QP) PostSend(wrs ...SendWR) error {
	return &OpError{Op: "ibv_post_send", Err: syscall.ENOSYS}
}

// PostRecv posts receive work requests as one chain; after it returns the C
// side keeps no reference to Go memory.
func (q *QP) PostRecv(wrs ...RecvWR) error {
	return &OpError{Op: "ibv_post_recv", Err: syscall.ENOSYS}
}

// Close destroys the queue pair. A second Close is a no-op.
func (q *QP) Close() error {
	qp := q.qp
	if qp == nil {
		return nil
	}
	if q.owned {
		if rc := C.ibv_destroy_qp(qp); rc != 0 {
			return opErrorRC("ibv_destroy_qp", rc)
		}
		qps.Delete(uintptr(unsafe.Pointer(qp)))
	}
	q.qp = nil
	return nil
}

func capToC(c QPCap) C.struct_ibv_qp_cap {
	return C.struct_ibv_qp_cap{
		max_send_wr:     C.uint32_t(c.MaxSendWR),
		max_recv_wr:     C.uint32_t(c.MaxRecvWR),
		max_send_sge:    C.uint32_t(c.MaxSendSGE),
		max_recv_sge:    C.uint32_t(c.MaxRecvSGE),
		max_inline_data: C.uint32_t(c.MaxInlineData),
	}
}

func capFromC(c C.struct_ibv_qp_cap) QPCap {
	return QPCap{
		MaxSendWR:     uint32(c.max_send_wr),
		MaxRecvWR:     uint32(c.max_recv_wr),
		MaxSendSGE:    uint32(c.max_send_sge),
		MaxRecvSGE:    uint32(c.max_recv_sge),
		MaxInlineData: uint32(c.max_inline_data),
	}
}

func ahToC(a AHAttr) C.struct_ibv_ah_attr {
	ca := C.struct_ibv_ah_attr{
		dlid:          C.uint16_t(a.DLID),
		sl:            C.uint8_t(a.SL),
		src_path_bits: C.uint8_t(a.SrcPathBits),
		static_rate:   C.uint8_t(a.StaticRate),
		port_num:      C.uint8_t(a.Port),
	}
	if a.IsGlobal {
		ca.is_global = 1
	}
	ca.grh.flow_label = C.uint32_t(a.GRH.FlowLabel)
	ca.grh.sgid_index = C.uint8_t(a.GRH.SGIDIndex)
	ca.grh.hop_limit = C.uint8_t(a.GRH.HopLimit)
	ca.grh.traffic_class = C.uint8_t(a.GRH.TrafficClass)
	*(*GID)(unsafe.Pointer(&ca.grh.dgid)) = a.GRH.DGID
	return ca
}

func ahFromC(ca *C.struct_ibv_ah_attr) AHAttr {
	return AHAttr{
		GRH: GlobalRoute{
			DGID:         *(*GID)(unsafe.Pointer(&ca.grh.dgid)),
			FlowLabel:    uint32(ca.grh.flow_label),
			SGIDIndex:    uint8(ca.grh.sgid_index),
			HopLimit:     uint8(ca.grh.hop_limit),
			TrafficClass: uint8(ca.grh.traffic_class),
		},
		DLID:        uint16(ca.dlid),
		SL:          uint8(ca.sl),
		SrcPathBits: uint8(ca.src_path_bits),
		StaticRate:  uint8(ca.static_rate),
		IsGlobal:    ca.is_global != 0,
		Port:        uint8(ca.port_num),
	}
}

func attrToC(a *QPAttr) C.struct_ibv_qp_attr {
	ca := C.struct_ibv_qp_attr{
		qp_state:           uint32(a.State),
		cur_qp_state:       uint32(a.CurState),
		path_mtu:           uint32(a.PathMTU),
		path_mig_state:     uint32(a.PathMigState),
		qkey:               C.uint32_t(a.QKey),
		rq_psn:             C.uint32_t(a.RQPSN),
		sq_psn:             C.uint32_t(a.SQPSN),
		dest_qp_num:        C.uint32_t(a.DestQPNum),
		qp_access_flags:    C.uint(a.AccessFlags),
		cap:                capToC(a.Cap),
		ah_attr:            ahToC(a.AH),
		alt_ah_attr:        ahToC(a.AltAH),
		pkey_index:         C.uint16_t(a.PkeyIndex),
		alt_pkey_index:     C.uint16_t(a.AltPkeyIndex),
		max_rd_atomic:      C.uint8_t(a.MaxRDAtomic),
		max_dest_rd_atomic: C.uint8_t(a.MaxDestRDAtomic),
		min_rnr_timer:      C.uint8_t(a.MinRNRTimer),
		port_num:           C.uint8_t(a.Port),
		timeout:            C.uint8_t(a.Timeout),
		retry_cnt:          C.uint8_t(a.RetryCnt),
		rnr_retry:          C.uint8_t(a.RNRRetry),
		alt_port_num:       C.uint8_t(a.AltPort),
		alt_timeout:        C.uint8_t(a.AltTimeout),
		rate_limit:         C.uint32_t(a.RateLimit),
	}
	if a.EnSQDAsyncNotify {
		ca.en_sqd_async_notify = 1
	}
	if a.SQDraining {
		ca.sq_draining = 1
	}
	return ca
}

func attrFromC(ca *C.struct_ibv_qp_attr) *QPAttr {
	return &QPAttr{
		State:            QPState(ca.qp_state),
		CurState:         QPState(ca.cur_qp_state),
		PathMTU:          MTU(ca.path_mtu),
		PathMigState:     MigState(ca.path_mig_state),
		QKey:             uint32(ca.qkey),
		RQPSN:            uint32(ca.rq_psn),
		SQPSN:            uint32(ca.sq_psn),
		DestQPNum:        uint32(ca.dest_qp_num),
		AccessFlags:      AccessFlags(ca.qp_access_flags),
		Cap:              capFromC(ca.cap),
		AH:               ahFromC(&ca.ah_attr),
		AltAH:            ahFromC(&ca.alt_ah_attr),
		PkeyIndex:        uint16(ca.pkey_index),
		AltPkeyIndex:     uint16(ca.alt_pkey_index),
		EnSQDAsyncNotify: ca.en_sqd_async_notify != 0,
		SQDraining:       ca.sq_draining != 0,
		MaxRDAtomic:      uint8(ca.max_rd_atomic),
		MaxDestRDAtomic:  uint8(ca.max_dest_rd_atomic),
		MinRNRTimer:      uint8(ca.min_rnr_timer),
		Port:             uint8(ca.port_num),
		Timeout:          uint8(ca.timeout),
		RetryCnt:         uint8(ca.retry_cnt),
		RNRRetry:         uint8(ca.rnr_retry),
		AltPort:          uint8(ca.alt_port_num),
		AltTimeout:       uint8(ca.alt_timeout),
		RateLimit:        uint32(ca.rate_limit),
	}
}
