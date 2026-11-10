//go:build linux && !android && (amd64 || arm64 || loong64 || ppc64le || riscv64 || s390x)

package cm

import (
	"errors"
	"net/netip"
	"syscall"
	"testing"
	"unsafe"

	"github.com/wzshiming/rdma/internal/rdmaabi"
	"github.com/wzshiming/rdma/verbs"
)

// bytesOf returns the memory of ss.
func bytesOf(ss *sockaddrStorage) []byte {
	return unsafe.Slice((*byte)(unsafe.Pointer(ss)), unsafe.Sizeof(*ss))
}

// TestSockaddrPortIsNetworkOrder checks the sockaddr handed to librdmacm:
// the family as the host's sa_family_t, the port in network order whatever
// the host's byte order, and the address bytes after them.
func TestSockaddrPortIsNetworkOrder(t *testing.T) {
	var ss sockaddrStorage
	if unsafe.Sizeof(ss) != 128 || unsafe.Alignof(ss) != 8 {
		t.Fatalf("sockaddrStorage is %d bytes aligned to %d, want 128 aligned to 8 like struct sockaddr_storage", unsafe.Sizeof(ss), unsafe.Alignof(ss))
	}
	ss.set(netip.MustParseAddrPort("10.200.0.1:4791"))
	raw := bytesOf(&ss)
	if ss.family != syscall.AF_INET || raw[2] != 0x12 || raw[3] != 0xb7 || string(raw[4:8]) != "\x0a\xc8\x00\x01" {
		t.Errorf("sockaddr_in = % x, want family AF_INET, port 12 b7, address 0a c8 00 01", raw[:16])
	}
	ss.set(netip.MustParseAddrPort("[2001:db8::1]:7471"))
	raw = bytesOf(&ss)
	if ss.family != syscall.AF_INET6 || raw[2] != 0x1d || raw[3] != 0x2f || string(raw[8:24]) != "\x20\x01\x0d\xb8"+"\x00\x00\x00\x00\x00\x00\x00\x00\x00\x00\x00\x01" {
		t.Errorf("sockaddr_in6 = % x, want family AF_INET6, port 1d 2f, address 2001:db8::1 at offset 8", raw[:28])
	}
}

func TestSockaddrRoundTrip(t *testing.T) {
	for _, tt := range []struct {
		in, want string
	}{
		{"10.200.0.1:4791", "10.200.0.1:4791"},
		{"[2001:db8::1]:4791", "[2001:db8::1]:4791"},
		{"[::ffff:10.200.0.1]:4791", "10.200.0.1:4791"}, // v4-mapped becomes AF_INET
		{"[::]:0", "[::]:0"},
		{"0.0.0.0:0", "0.0.0.0:0"},
	} {
		var ss sockaddrStorage
		ss.set(netip.MustParseAddrPort(tt.in))
		if got := ss.addrPort(); got != netip.MustParseAddrPort(tt.want) {
			t.Errorf("addrPort(set(%s)) = %v, want %s", tt.in, got, tt.want)
		}
	}
	var ss sockaddrStorage
	ss.family = syscall.AF_UNIX
	if got := ss.addrPort(); got.IsValid() {
		t.Errorf("addrPort() of an AF_UNIX sockaddr = %v, want the zero value", got)
	}
}

// TestRouteAddrsReadTheIDStorage reads the addresses as rdma_get_local_addr
// and rdma_get_peer_addr do: from route.addr.src_addr and dst_addr of the
// identifier.
func TestRouteAddrsReadTheIDStorage(t *testing.T) {
	var id rdmaabi.RdmaCmId
	var ss sockaddrStorage
	ss.set(netip.MustParseAddrPort("10.200.0.1:4791"))
	copy(id.Route.Addr.Src_storage[:], bytesOf(&ss))
	ss.set(netip.MustParseAddrPort("[2001:db8::2]:7471"))
	copy(id.Route.Addr.Dst_storage[:], bytesOf(&ss))
	local, remote := addrs(&id)
	if local != netip.MustParseAddrPort("10.200.0.1:4791") || remote != netip.MustParseAddrPort("[2001:db8::2]:7471") {
		t.Errorf("addrs() = %v, %v; want 10.200.0.1:4791, [2001:db8::2]:7471", local, remote)
	}
}

// TestConnParamCopiesPrivateData checks the struct rdma_conn_param of a
// connect or accept: the parameters as given and the private data copied
// into the same object, where the call finds it.
func TestConnParamCopiesPrivateData(t *testing.T) {
	cp := newConnParam(ConnParam{PrivateData: []byte("hello"), ResponderResources: 2, InitiatorDepth: 3, RetryCount: 4, RNRRetryCount: 5})
	c := &cp.c
	if c.Responder_resources != 2 || c.Initiator_depth != 3 || c.Retry_count != 4 || c.Rnr_retry_count != 5 {
		t.Errorf("rdma_conn_param = %+v, want responder_resources 2, initiator_depth 3, retry_count 4, rnr_retry_count 5", *c)
	}
	if c.Private_data_len != 5 || c.Private_data != uint64(uintptr(unsafe.Pointer(&cp.data))) || string(cp.data[:5]) != "hello" {
		t.Errorf("private data: len %d at %#x, data %q; want 5 bytes %q at %#x", c.Private_data_len, c.Private_data, cp.data[:5], "hello", uintptr(unsafe.Pointer(&cp.data)))
	}
	if c.Flow_control != 0 || c.Srq != 0 || c.Qp_num != 0 {
		t.Errorf("rdma_conn_param = %+v, want flow_control, srq and qp_num zero", *c)
	}
	cp = newConnParam(ConnParam{ResponderResources: 1})
	if cp.c.Private_data != 0 || cp.c.Private_data_len != 0 {
		t.Errorf("without private data: pointer %#x, len %d; want NULL and 0", cp.c.Private_data, cp.c.Private_data_len)
	}
	if len(cp.data) < MaxConnectPrivateData || len(cp.data) < MaxAcceptPrivateData {
		t.Errorf("data holds %d bytes, want room for %d (connect) and %d (accept)", len(cp.data), MaxConnectPrivateData, MaxAcceptPrivateData)
	}
}

// TestEventFromCCopiesTheFields checks the copy of a struct rdma_cm_event
// taken before the event is acknowledged.
func TestEventFromCCopiesTheFields(t *testing.T) {
	var cev rdmaabi.RdmaCmEvent
	cev.Event = uint32(EventRejected)
	cev.Status = 28
	cev.Param.Conn.Responder_resources = 2
	cev.Param.Conn.Initiator_depth = 3
	ev := eventFromC(&cev)
	if ev.typ != EventRejected || ev.status != 28 || ev.responderResources != 2 || ev.initiatorDepth != 3 {
		t.Errorf("eventFromC() = %+v, want REJECTED, status 28, responder_resources 2, initiator_depth 3", *ev)
	}
	if ev.id != nil || ev.privateData != nil {
		t.Errorf("eventFromC() = %+v, want no id and no private data for a NULL id and private_data_len 0", *ev)
	}
}

// TestEventErrMapsStatus pins the error a connection attempt ends with: an
// explicit reject carries its data, the other events the errno a socket
// would produce, with the kernel's negative errno values passed on.
func TestEventErrMapsStatus(t *testing.T) {
	tests := []struct {
		name string
		ev   cmEvent
		want error
	}{
		{"rejected by Request.Reject", cmEvent{typ: EventRejected, status: ibCMRejConsumerDefined, privateData: []byte("no")}, &RejectedError{PrivateData: []byte("no"), Reason: 28}},
		{"rejected by the CM", cmEvent{typ: EventRejected, status: 8}, syscall.ECONNREFUSED},
		{"address error from the kernel", cmEvent{typ: EventAddrError, status: -int(syscall.EADDRNOTAVAIL)}, syscall.EADDRNOTAVAIL},
		{"route error without a status", cmEvent{typ: EventRouteError}, syscall.EHOSTUNREACH},
		{"unreachable from the kernel", cmEvent{typ: EventUnreachable, status: -int(syscall.ETIMEDOUT)}, syscall.ETIMEDOUT},
		{"connect error from librdmacm", cmEvent{typ: EventConnectError, status: -1}, syscall.ETIMEDOUT},
		{"disconnected", cmEvent{typ: EventDisconnected}, syscall.ECONNRESET},
		{"device removal", cmEvent{typ: EventDeviceRemoval}, syscall.ENODEV},
		{"established", cmEvent{typ: EventEstablished}, nil},
		{"address resolved", cmEvent{typ: EventAddrResolved}, nil},
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
