//go:build linux && !android && (amd64 || arm64 || loong64 || ppc64le || riscv64 || s390x)

package cm

import (
	"errors"
	"reflect"
	"syscall"
	"testing"

	"github.com/wzshiming/rdma/internal/rdmaabi"
	"github.com/wzshiming/rdma/internal/ucma"
	"github.com/wzshiming/rdma/verbs"
)

// TestQPAttrFromKernel pins the conversion of the attributes INIT_QP_ATTR
// returns, field by field, as ibv_copy_qp_attr_from_kern makes it: every
// field of struct ib_uverbs_qp_attr lands in its verbs.QPAttr field, the
// state is the one asked for, and the mask is the kernel's.
func TestQPAttrFromKernel(t *testing.T) {
	k := rdmaabi.IbUverbsQpAttr{
		Qp_attr_mask: 0x1fffff, Qp_state: uint32(verbs.QPStateErr), Cur_qp_state: 1, Path_mtu: 3, Path_mig_state: 2,
		Qkey: 0x11111111, Rq_psn: 0x222222, Sq_psn: 0x333333, Dest_qp_num: 0x444444, Qp_access_flags: 0xe,
		Ah_attr: rdmaabi.IbUverbsAhAttr{
			Grh:  rdmaabi.IbUverbsGlobalRoute{Dgid: [16]byte{0xfe, 0x80, 15: 1}, Flow_label: 0x55555, Sgid_index: 6, Hop_limit: 64, Traffic_class: 7},
			Dlid: 0x88, Sl: 9, Src_path_bits: 10, Static_rate: 11, Is_global: 1, Port_num: 12,
		},
		Alt_ah_attr: rdmaabi.IbUverbsAhAttr{
			Grh:  rdmaabi.IbUverbsGlobalRoute{Dgid: [16]byte{0xff, 0x02, 15: 2}, Flow_label: 0x66666, Sgid_index: 13, Hop_limit: 14, Traffic_class: 15},
			Dlid: 0x99, Sl: 16, Src_path_bits: 17, Static_rate: 18, Port_num: 19,
		},
		Max_send_wr: 21, Max_recv_wr: 22, Max_send_sge: 23, Max_recv_sge: 24, Max_inline_data: 25,
		Pkey_index: 26, Alt_pkey_index: 27, En_sqd_async_notify: 1, Sq_draining: 1,
		Max_rd_atomic: 28, Max_dest_rd_atomic: 29, Min_rnr_timer: 30, Port_num: 31, Timeout: 32,
		Retry_cnt: 33, Rnr_retry: 34, Alt_port_num: 35, Alt_timeout: 36,
	}
	got, mask := qpAttrFromKernel(verbs.QPStateRTR, &k)
	if mask != 0x1fffff {
		t.Errorf("mask = %#x, want the kernel's %#x", uint32(mask), k.Qp_attr_mask)
	}
	want := verbs.QPAttr{
		State: verbs.QPStateRTR, CurState: 1, PathMTU: 3, PathMigState: 2,
		QKey: 0x11111111, RQPSN: 0x222222, SQPSN: 0x333333, DestQPNum: 0x444444, AccessFlags: 0xe,
		Cap: verbs.QPCap{MaxSendWR: 21, MaxRecvWR: 22, MaxSendSGE: 23, MaxRecvSGE: 24, MaxInlineData: 25},
		AH: verbs.AHAttr{
			GRH:  verbs.GlobalRoute{DGID: verbs.GID{0xfe, 0x80, 15: 1}, FlowLabel: 0x55555, SGIDIndex: 6, HopLimit: 64, TrafficClass: 7},
			DLID: 0x88, SL: 9, SrcPathBits: 10, StaticRate: 11, IsGlobal: true, Port: 12,
		},
		AltAH: verbs.AHAttr{
			GRH:  verbs.GlobalRoute{DGID: verbs.GID{0xff, 0x02, 15: 2}, FlowLabel: 0x66666, SGIDIndex: 13, HopLimit: 14, TrafficClass: 15},
			DLID: 0x99, SL: 16, SrcPathBits: 17, StaticRate: 18, Port: 19,
		},
		PkeyIndex: 26, AltPkeyIndex: 27, EnSQDAsyncNotify: true, SQDraining: true,
		MaxRDAtomic: 28, MaxDestRDAtomic: 29, MinRNRTimer: 30, Port: 31, Timeout: 32,
		RetryCnt: 33, RNRRetry: 34, AltPort: 35, AltTimeout: 36,
	}
	g, w := reflect.ValueOf(*got), reflect.ValueOf(want)
	for i := range g.NumField() {
		if !reflect.DeepEqual(g.Field(i).Interface(), w.Field(i).Interface()) {
			t.Errorf("%s = %+v, want %+v", g.Type().Field(i).Name, g.Field(i), w.Field(i))
		}
	}
}

// TestRTRMask pins librdmacm's workaround for an old rdma_ucm: the mask of
// the RTR transition loses bits 21 to 23, which that kernel sets for RoCE,
// on an InfiniBand link layer only.
func TestRTRMask(t *testing.T) {
	for _, tt := range []struct {
		name     string
		link     verbs.LinkLayer
		in, want verbs.QPAttrMask
	}{
		{"InfiniBand", verbs.LinkLayerInfiniBand, 0x1ffffff, 0x11fffff},
		{"Ethernet", verbs.LinkLayerEthernet, 0x1ffffff, 0x1ffffff},
		{"unspecified", verbs.LinkLayerUnspecified, 0xe00001, 0xe00001},
		{"InfiniBand without the bits", verbs.LinkLayerInfiniBand, 0x1fffff, 0x1fffff},
	} {
		if got := rtrMask(tt.in, tt.link); got != tt.want {
			t.Errorf("%s: rtrMask(%#x) = %#x, want %#x", tt.name, uint32(tt.in), uint32(got), uint32(tt.want))
		}
	}
}

// TestDepthOverrides pins the depths of a connection overriding the
// kernel's attributes: the responder resources in max_dest_rd_atomic of RTR,
// the initiator depth in max_rd_atomic of RTS, unless the 0xFF that leaves
// the kernel's value.
func TestDepthOverrides(t *testing.T) {
	const kernRTR, kernRTS = 40, 50 // the kernel's max_dest_rd_atomic and max_rd_atomic
	for _, tt := range []struct {
		name             string
		depth            uint8
		wantRTR, wantRTS uint8
	}{
		{"a depth", 3, 3, 3},
		{"zero", 0, 0, 0},
		{"the 0xFF sentinel", 0xFF, kernRTR, kernRTS},
	} {
		rtr := verbs.QPAttr{MaxDestRDAtomic: kernRTR, MaxRDAtomic: kernRTS}
		overrideRTR(&rtr, tt.depth)
		if rtr.MaxDestRDAtomic != tt.wantRTR || rtr.MaxRDAtomic != kernRTS {
			t.Errorf("%s: overrideRTR(%d) = max_dest_rd_atomic %d, max_rd_atomic %d; want %d, %d", tt.name, tt.depth, rtr.MaxDestRDAtomic, rtr.MaxRDAtomic, tt.wantRTR, kernRTS)
		}
		rts := verbs.QPAttr{MaxDestRDAtomic: kernRTR, MaxRDAtomic: kernRTS}
		overrideRTS(&rts, tt.depth)
		if rts.MaxRDAtomic != tt.wantRTS || rts.MaxDestRDAtomic != kernRTR {
			t.Errorf("%s: overrideRTS(%d) = max_rd_atomic %d, max_dest_rd_atomic %d; want %d, %d", tt.name, tt.depth, rts.MaxRDAtomic, rts.MaxDestRDAtomic, tt.wantRTS, kernRTR)
		}
	}
}

// TestCheckDepths pins ucma_valid_param: a depth above the device's limit
// is EINVAL, the 0xFF sentinel never is.
func TestCheckDepths(t *testing.T) {
	lim := limits{initiatorDepth: 16, responderResources: 8}
	for _, tt := range []struct {
		name string
		p    ConnParam
		want error
	}{
		{"within the limits", ConnParam{ResponderResources: 8, InitiatorDepth: 16}, nil},
		{"responder resources above", ConnParam{ResponderResources: 9, InitiatorDepth: 1}, syscall.EINVAL},
		{"initiator depth above", ConnParam{ResponderResources: 1, InitiatorDepth: 17}, syscall.EINVAL},
		{"the sentinels", ConnParam{ResponderResources: 0xFF, InitiatorDepth: 0xFF}, nil},
	} {
		if got := checkDepths(tt.p, lim); got != tt.want {
			t.Errorf("%s: checkDepths(%+v) = %v, want %v", tt.name, tt.p, got, tt.want)
		}
	}
}

// TestConnectDepths pins the depths rdma_connect keeps for a connection:
// those given, the device's limits for the 0xFF sentinel.
func TestConnectDepths(t *testing.T) {
	lim := limits{initiatorDepth: 16, responderResources: 8}
	for _, tt := range []struct {
		name                     string
		p                        ConnParam
		wantInitiator, wantResps uint8
	}{
		{"given", ConnParam{ResponderResources: 2, InitiatorDepth: 3}, 3, 2},
		{"the sentinels", ConnParam{ResponderResources: 0xFF, InitiatorDepth: 0xFF}, 16, 8},
	} {
		initiator, responder := connectDepths(tt.p, lim)
		if initiator != tt.wantInitiator || responder != tt.wantResps {
			t.Errorf("%s: connectDepths(%+v) = %d, %d; want %d, %d", tt.name, tt.p, initiator, responder, tt.wantInitiator, tt.wantResps)
		}
	}
}

// TestAcceptDepths pins the depths rdma_accept keeps: those given, for the
// 0xFF sentinel the request's within the device's limits.
func TestAcceptDepths(t *testing.T) {
	lim := limits{initiatorDepth: 16, responderResources: 8}
	for _, tt := range []struct {
		name                     string
		p                        ConnParam
		reqInitiator, reqResps   uint8
		wantInitiator, wantResps uint8
	}{
		{"given", ConnParam{ResponderResources: 2, InitiatorDepth: 3}, 5, 6, 3, 2},
		{"the sentinels and a small request", ConnParam{ResponderResources: 0xFF, InitiatorDepth: 0xFF}, 5, 6, 5, 6},
		{"the sentinels and a large request", ConnParam{ResponderResources: 0xFF, InitiatorDepth: 0xFF}, 100, 100, 16, 8},
	} {
		initiator, responder := acceptDepths(tt.p, tt.reqInitiator, tt.reqResps, lim)
		if initiator != tt.wantInitiator || responder != tt.wantResps {
			t.Errorf("%s: acceptDepths(%+v, %d, %d) = %d, %d; want %d, %d", tt.name, tt.p, tt.reqInitiator, tt.reqResps, initiator, responder, tt.wantInitiator, tt.wantResps)
		}
	}
}

// TestKernelConnParam pins the connection parameters of CONNECT and ACCEPT
// as ucma_copy_conn_param_to_kern writes them: the queue pair, the depths
// kept for the connection, the retry counts and private data as given, no
// SRQ and no flow control.
func TestKernelConnParam(t *testing.T) {
	p := ConnParam{PrivateData: []byte("hello"), ResponderResources: 2, InitiatorDepth: 3, RetryCount: 4, RNRRetryCount: 5}
	got := kernelConnParam(p, 0x1234, 6, 7)
	want := ucma.ConnParam{PrivateData: []byte("hello"), QPNum: 0x1234, InitiatorDepth: 6, ResponderResources: 7, RetryCount: 4, RNRRetryCount: 5}
	if !reflect.DeepEqual(got, want) {
		t.Errorf("kernelConnParam() = %+v, want %+v", got, want)
	}
}

// TestEventErr pins the error a connection attempt ends with: an explicit
// reject carries its data, the other events the errno a socket would
// produce, with the kernel's negative errno values passed on.
func TestEventErr(t *testing.T) {
	tests := []struct {
		name string
		ev   event
		want error
	}{
		{"rejected by Request.Reject", event{typ: EventRejected, status: ibCMRejConsumerDefined, conn: ucma.ConnParam{PrivateData: []byte("no")}}, &RejectedError{PrivateData: []byte("no"), Reason: 28}},
		{"rejected by the CM", event{typ: EventRejected, status: 8}, syscall.ECONNREFUSED},
		{"address error from the kernel", event{typ: EventAddrError, status: -int(syscall.EADDRNOTAVAIL)}, syscall.EADDRNOTAVAIL},
		{"address error of the connection manager", event{typ: EventAddrError, status: -1}, syscall.EHOSTUNREACH},
		{"route error without a status", event{typ: EventRouteError}, syscall.EHOSTUNREACH},
		{"unreachable from the kernel", event{typ: EventUnreachable, status: -int(syscall.ETIMEDOUT)}, syscall.ETIMEDOUT},
		{"connect error of the connection manager", event{typ: EventConnectError, status: -1}, syscall.ETIMEDOUT},
		{"disconnected", event{typ: EventDisconnected}, syscall.ECONNRESET},
		{"device removal", event{typ: EventDeviceRemoval}, syscall.ENODEV},
		{"established", event{typ: EventEstablished}, nil},
		{"address resolved", event{typ: EventAddrResolved}, nil},
		{"timewait exit", event{typ: EventTimewaitExit}, nil},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			err := tt.ev.err("rdma_connect")
			if tt.want == nil {
				if err != nil {
					t.Fatalf("err() = %v, want nil", err)
				}
				return
			}
			if rej, ok := tt.want.(*RejectedError); ok {
				var got *RejectedError
				if !errors.As(err, &got) || string(got.PrivateData) != string(rej.PrivateData) || got.Reason != rej.Reason {
					t.Fatalf("err() = %#v, want %#v", err, rej)
				}
				return
			}
			var opErr *verbs.OpError
			if !errors.As(err, &opErr) || opErr.Op != "rdma_connect" || opErr.Err != tt.want {
				t.Fatalf("err() = %#v, want a *verbs.OpError of rdma_connect with %v", err, tt.want)
			}
			var rej *RejectedError
			if errors.As(err, &rej) {
				t.Errorf("err() = %v is a *RejectedError, want a plain errno", err)
			}
		})
	}
}

// TestMatchDevice pins librdmacm's device match: by the kernel's index and
// the node GUID, by the GUID alone when either side does not know the index.
func TestMatchDevice(t *testing.T) {
	for _, tt := range []struct {
		name                string
		devGUID, guid       uint64
		devIndex, kernIndex int
		want                bool
	}{
		{"same index and GUID", 0xa, 0xa, 1, 1, true},
		{"same GUID, other index", 0xa, 0xa, 0, 1, false},
		{"same index, other GUID", 0xa, 0xb, 1, 1, false},
		{"the device's index unknown", 0xa, 0xa, -1, 1, true},
		{"the kernel's index unknown", 0xa, 0xa, 1, -1, true},
		{"index unknown, other GUID", 0xa, 0xb, -1, 1, false},
	} {
		if got := matchDevice(tt.devGUID, tt.devIndex, tt.guid, tt.kernIndex); got != tt.want {
			t.Errorf("%s: matchDevice(%#x, %d, %#x, %d) = %v, want %v", tt.name, tt.devGUID, tt.devIndex, tt.guid, tt.kernIndex, got, tt.want)
		}
	}
}

// TestShutdownState pins the state rdma_disconnect moves the queue pair to
// before it disconnects: ERR on the InfiniBand transport, RoCE included,
// SQD on iWARP, EINVAL on any other.
func TestShutdownState(t *testing.T) {
	for _, tt := range []struct {
		transport verbs.TransportType
		state     verbs.QPState
		err       error
	}{
		{verbs.TransportIB, verbs.QPStateErr, nil},
		{verbs.TransportIWARP, verbs.QPStateSQD, nil},
		{verbs.TransportUSNIC, 0, syscall.EINVAL},
		{verbs.TransportUnknown, 0, syscall.EINVAL},
	} {
		state, err := shutdownState(tt.transport)
		if state != tt.state || err != tt.err {
			t.Errorf("shutdownState(%v) = %v, %v; want %v, %v", tt.transport, state, err, tt.state, tt.err)
		}
	}
}

// TestKnownEvent pins whose events an identifier takes, as
// rdma_get_cm_event decides: those with its uid; of those with uid 0, which
// an old kernel reports, only ESTABLISHED for its kernel id.
func TestKnownEvent(t *testing.T) {
	const uid, handle = 7, 3
	for _, tt := range []struct {
		name string
		ev   ucma.Event
		want bool
	}{
		{"its uid", ucma.Event{Type: ucma.EventDisconnected, UID: uid, ID: handle}, true},
		{"another uid", ucma.Event{Type: ucma.EventDisconnected, UID: 8, ID: handle}, false},
		{"uid 0, ESTABLISHED of its id", ucma.Event{Type: ucma.EventEstablished, ID: handle}, true},
		{"uid 0, ESTABLISHED of another id", ucma.Event{Type: ucma.EventEstablished, ID: 4}, false},
		{"uid 0, another event of its id", ucma.Event{Type: ucma.EventDisconnected, ID: handle}, false},
	} {
		if got := known(tt.ev, uid, handle); got != tt.want {
			t.Errorf("%s: known(%+v) = %v, want %v", tt.name, tt.ev, got, tt.want)
		}
	}
}
