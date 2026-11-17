//go:build linux && !android && (amd64 || arm64 || loong64 || ppc64le || riscv64 || s390x)

package cm

import (
	"syscall"

	"github.com/wzshiming/rdma/internal/rdmaabi"
	"github.com/wzshiming/rdma/internal/ucma"
	"github.com/wzshiming/rdma/verbs"
)

// ibCMRejConsumerDefined is the IB CM reject reason of an explicit
// rdma_reject (IB_CM_REJ_CONSUMER_DEFINED).
const ibCMRejConsumerDefined = 28

// maxDepth is the depth that stands for the device's limit
// (RDMA_MAX_RESP_RES and RDMA_MAX_INIT_DEPTH).
const maxDepth = 0xFF

// limits are what librdmacm checks the depths of a connection against and
// fills the sentinel with: max_qp_init_rd_atom and max_qp_rd_atom of the
// device, cut to a byte as librdmacm cuts them.
type limits struct {
	initiatorDepth     uint8
	responderResources uint8
}

// checkDepths checks the depths of p as ucma_valid_param does: one above
// the device's limit is EINVAL, the sentinel never is.
func checkDepths(p ConnParam, l limits) error {
	if p.ResponderResources != maxDepth && p.ResponderResources > l.responderResources ||
		p.InitiatorDepth != maxDepth && p.InitiatorDepth > l.initiatorDepth {
		return syscall.EINVAL
	}
	return nil
}

// connectDepths returns the depths rdma_connect keeps for a connection:
// those of p, the device's limits for the sentinel.
func connectDepths(p ConnParam, l limits) (initiator, responder uint8) {
	initiator, responder = p.InitiatorDepth, p.ResponderResources
	if initiator == maxDepth {
		initiator = l.initiatorDepth
	}
	if responder == maxDepth {
		responder = l.responderResources
	}
	return initiator, responder
}

// acceptDepths returns the depths rdma_accept keeps for a connection: those
// of p, for the sentinel the request's within the device's limits.
func acceptDepths(p ConnParam, reqInitiator, reqResponder uint8, l limits) (initiator, responder uint8) {
	initiator, responder = p.InitiatorDepth, p.ResponderResources
	if initiator == maxDepth {
		initiator = min(reqInitiator, l.initiatorDepth)
	}
	if responder == maxDepth {
		responder = min(reqResponder, l.responderResources)
	}
	return initiator, responder
}

// kernelConnParam is the connection form of CONNECT and ACCEPT as
// ucma_copy_conn_param_to_kern writes it: the queue pair qpNum, which has no
// SRQ, the depths kept for the connection, the retry counts and private data
// of p, and no flow control, which the package does not offer.
func kernelConnParam(p ConnParam, qpNum uint32, initiator, responder uint8) ucma.ConnParam {
	return ucma.ConnParam{
		PrivateData:        p.PrivateData,
		QPNum:              qpNum,
		ResponderResources: responder,
		InitiatorDepth:     initiator,
		RetryCount:         p.RetryCount,
		RNRRetryCount:      p.RNRRetryCount,
	}
}

// qpAttrFromKernel converts the attributes INIT_QP_ATTR returns for the
// transition to state as ibv_copy_qp_attr_from_kern does: every field but
// the state, which is the transition's; the mask is the kernel's.
func qpAttrFromKernel(state verbs.QPState, k *rdmaabi.IbUverbsQpAttr) (*verbs.QPAttr, verbs.QPAttrMask) {
	return &verbs.QPAttr{
		State:        state,
		CurState:     verbs.QPState(k.Cur_qp_state),
		PathMTU:      verbs.MTU(k.Path_mtu),
		PathMigState: verbs.MigState(k.Path_mig_state),
		QKey:         k.Qkey,
		RQPSN:        k.Rq_psn,
		SQPSN:        k.Sq_psn,
		DestQPNum:    k.Dest_qp_num,
		AccessFlags:  verbs.AccessFlags(k.Qp_access_flags),
		Cap: verbs.QPCap{
			MaxSendWR:     k.Max_send_wr,
			MaxRecvWR:     k.Max_recv_wr,
			MaxSendSGE:    k.Max_send_sge,
			MaxRecvSGE:    k.Max_recv_sge,
			MaxInlineData: k.Max_inline_data,
		},
		AH:               ahAttrFromKernel(&k.Ah_attr),
		AltAH:            ahAttrFromKernel(&k.Alt_ah_attr),
		PkeyIndex:        k.Pkey_index,
		AltPkeyIndex:     k.Alt_pkey_index,
		EnSQDAsyncNotify: k.En_sqd_async_notify != 0,
		SQDraining:       k.Sq_draining != 0,
		MaxRDAtomic:      k.Max_rd_atomic,
		MaxDestRDAtomic:  k.Max_dest_rd_atomic,
		MinRNRTimer:      k.Min_rnr_timer,
		Port:             k.Port_num,
		Timeout:          k.Timeout,
		RetryCnt:         k.Retry_cnt,
		RNRRetry:         k.Rnr_retry,
		AltPort:          k.Alt_port_num,
		AltTimeout:       k.Alt_timeout,
	}, verbs.QPAttrMask(k.Qp_attr_mask)
}

// ahAttrFromKernel converts an address vector as ibv_copy_ah_attr_from_kern
// does.
func ahAttrFromKernel(k *rdmaabi.IbUverbsAhAttr) verbs.AHAttr {
	return verbs.AHAttr{
		GRH: verbs.GlobalRoute{
			DGID:         verbs.GID(k.Grh.Dgid),
			FlowLabel:    k.Grh.Flow_label,
			SGIDIndex:    k.Grh.Sgid_index,
			HopLimit:     k.Grh.Hop_limit,
			TrafficClass: k.Grh.Traffic_class,
		},
		DLID:        k.Dlid,
		SL:          k.Sl,
		SrcPathBits: k.Src_path_bits,
		StaticRate:  k.Static_rate,
		IsGlobal:    k.Is_global != 0,
		Port:        k.Port_num,
	}
}

// rtrMask is the mask of the RTR transition: the kernel's, less bits 21 to
// 23 on an InfiniBand link layer, which librdmacm clears there because an
// old rdma_ucm sets them for RoCE.
func rtrMask(mask verbs.QPAttrMask, link verbs.LinkLayer) verbs.QPAttrMask {
	if link == verbs.LinkLayerInfiniBand {
		return mask &^ 0xe00000
	}
	return mask
}

// overrideRTR puts the responder resources of a connection into the
// attributes of its RTR transition, unless they are the sentinel.
func overrideRTR(a *verbs.QPAttr, responderResources uint8) {
	if responderResources != maxDepth {
		a.MaxDestRDAtomic = responderResources
	}
}

// overrideRTS puts the initiator depth of a connection into the attributes
// of its RTS transition, unless it is the sentinel.
func overrideRTS(a *verbs.QPAttr, initiatorDepth uint8) {
	if initiatorDepth != maxDepth {
		a.MaxRDAtomic = initiatorDepth
	}
}

// shutdownState is the state rdma_disconnect moves the queue pair to before
// it disconnects, as ucma_shutdown chooses it: ERR on the InfiniBand
// transport, which RoCE's is, SQD on iWARP; another transport is EINVAL.
func shutdownState(t verbs.TransportType) (verbs.QPState, error) {
	switch t {
	case verbs.TransportIB:
		return verbs.QPStateErr, nil
	case verbs.TransportIWARP:
		return verbs.QPStateSQD, nil
	}
	return 0, syscall.EINVAL
}

// matchDevice reports whether the device with node GUID devGUID and kernel
// index devIndex is the one the kernel names by guid and index, as
// librdmacm's match decides: by both, by the GUID alone when either index is
// unknown (-1).
func matchDevice(devGUID uint64, devIndex int, guid uint64, index int) bool {
	if devIndex < 0 || index < 0 {
		return devGUID == guid
	}
	return devIndex == index && devGUID == guid
}

// known reports whether ev, fetched from the channel of the identifier with
// uid and kernel id handle, is the identifier's as rdma_get_cm_event decides:
// by its uid; with uid 0, which an old kernel reports, only an ESTABLISHED
// event of the identifier's kernel id. The rest is discarded.
func known(ev ucma.Event, uid uint64, handle uint32) bool {
	if ev.UID != 0 {
		return ev.UID == uid
	}
	return ev.ID == handle && ev.Type == ucma.EventEstablished
}

// event is an event as rdma_get_cm_event hands it out, after the work
// librdmacm does on it, which may change its type and status.
type event struct {
	typ    Event
	status int
	conn   ucma.ConnParam
}

// err translates an event that ends a connection attempt into the error op
// reports; events that do not end it give nil.
func (ev *event) err(op string) error {
	switch ev.typ {
	case EventRejected:
		if ev.status == ibCMRejConsumerDefined {
			return &RejectedError{PrivateData: ev.conn.PrivateData, Reason: ev.status}
		}
		return &verbs.OpError{Op: op, Err: syscall.ECONNREFUSED}
	case EventAddrError, EventRouteError:
		return &verbs.OpError{Op: op, Err: ev.statusErrno(syscall.EHOSTUNREACH)}
	case EventUnreachable, EventConnectError:
		return &verbs.OpError{Op: op, Err: ev.statusErrno(syscall.ETIMEDOUT)}
	case EventDisconnected:
		return &verbs.OpError{Op: op, Err: syscall.ECONNRESET}
	case EventDeviceRemoval:
		return &verbs.OpError{Op: op, Err: syscall.ENODEV}
	}
	return nil
}

// statusErrno reads the errno of an error event: the kernel reports negative
// errno values; def covers the rest, among them the -1 of a failure in the
// work the connection manager does on an event, as librdmacm reports it.
func (ev *event) statusErrno(def syscall.Errno) syscall.Errno {
	if ev.status < -1 {
		return syscall.Errno(-ev.status)
	}
	return def
}
