package cm

import (
	"fmt"
	"net/netip"
	"strconv"
	"syscall"

	"github.com/wzshiming/rdma/verbs"
)

// Private data limits of the IB CM messages rdma_cm sends: a connect request
// has 92 bytes of which rdma_cm uses 36 for its own header, a reply 196, a
// reject 148. The peer receives the whole field, zero padded.
const (
	MaxConnectPrivateData = 56
	MaxAcceptPrivateData  = 196
	MaxRejectPrivateData  = 148
)

// ConnParam holds the parameters of ID.Connect and Request.Accept (struct
// rdma_conn_param). A zero ResponderResources, InitiatorDepth, RetryCount or
// RNRRetryCount means the RC default: 1, 1, 7 and 7.
type ConnParam struct {
	PrivateData        []byte // at most MaxConnectPrivateData or MaxAcceptPrivateData bytes
	ResponderResources uint8  // incoming RDMA reads and atomics served at once
	InitiatorDepth     uint8  // outgoing RDMA reads and atomics in flight at once
	RetryCount         uint8  // 0..7, ignored when accepting
	RNRRetryCount      uint8  // 0..7, 7 retries forever
}

// withDefaults returns p, or the zero ConnParam when p is nil, with the RC
// defaults filled in for zero fields.
func (p *ConnParam) withDefaults() ConnParam {
	var q ConnParam
	if p != nil {
		q = *p
	}
	if q.ResponderResources == 0 {
		q.ResponderResources = 1
	}
	if q.InitiatorDepth == 0 {
		q.InitiatorDepth = 1
	}
	if q.RetryCount == 0 {
		q.RetryCount = 7
	}
	if q.RNRRetryCount == 0 {
		q.RNRRetryCount = 7
	}
	return q
}

// ConnInfo is what ID.Connect learns about the established connection: the
// private data of the peer's accept and the negotiated parameters.
type ConnInfo struct {
	PrivateData        []byte // MaxAcceptPrivateData bytes, zero padded
	ResponderResources uint8
	InitiatorDepth     uint8
}

// Request is a connection request taken from a Listener. ID is the incoming
// connection, already on an event channel of its own; it must be closed by
// the caller unless the request is rejected.
type Request struct {
	ID                 *ID
	PrivateData        []byte // MaxConnectPrivateData bytes, zero padded
	ResponderResources uint8
	InitiatorDepth     uint8
}

// RejectedError is the error ID.Connect returns when the peer rejected the
// connection explicitly with Request.Reject. It matches syscall.ECONNREFUSED
// in errors.Is, like the error for a port nobody listens on.
type RejectedError struct {
	PrivateData []byte // MaxRejectPrivateData bytes, zero padded
	Reason      int    // IB CM reject reason, 28 (consumer defined) for Request.Reject
}

func (e *RejectedError) Error() string {
	return "cm: connection rejected by peer (reason " + strconv.Itoa(e.Reason) + ")"
}

// Is reports whether target is syscall.ECONNREFUSED.
func (e *RejectedError) Is(target error) bool { return target == syscall.ECONNREFUSED }

// Event is the type of a connection manager event (RDMA_CM_EVENT_*).
type Event int

// Event types.
const (
	EventAddrResolved    Event = 0
	EventAddrError       Event = 1
	EventRouteResolved   Event = 2
	EventRouteError      Event = 3
	EventConnectRequest  Event = 4
	EventConnectResponse Event = 5
	EventConnectError    Event = 6
	EventUnreachable     Event = 7
	EventRejected        Event = 8
	EventEstablished     Event = 9
	EventDisconnected    Event = 10
	EventDeviceRemoval   Event = 11
	EventMulticastJoin   Event = 12
	EventMulticastError  Event = 13
	EventAddrChange      Event = 14
	EventTimewaitExit    Event = 15
)

var eventNames = []string{
	"ADDR_RESOLVED", "ADDR_ERROR", "ROUTE_RESOLVED", "ROUTE_ERROR", "CONNECT_REQUEST", "CONNECT_RESPONSE",
	"CONNECT_ERROR", "UNREACHABLE", "REJECTED", "ESTABLISHED", "DISCONNECTED", "DEVICE_REMOVAL",
	"MULTICAST_JOIN", "MULTICAST_ERROR", "ADDR_CHANGE", "TIMEWAIT_EXIT",
}

func (e Event) String() string {
	if e >= 0 && int(e) < len(eventNames) {
		return eventNames[e]
	}
	return "Event(" + strconv.Itoa(int(e)) + ")"
}

// invalidArg reports an argument rejected before any rdma_cm call as an
// *verbs.OpError of op that matches syscall.EINVAL.
func invalidArg(op string, format string, args ...any) error {
	return &verbs.OpError{Op: op, Err: fmt.Errorf(format+": %w", append(args, syscall.EINVAL)...)}
}

// checkPrivateData rejects private data longer than max.
func checkPrivateData(op string, data []byte, max int) error {
	if len(data) > max {
		return invalidArg(op, "%d bytes of private data exceed the limit of %d", len(data), max)
	}
	return nil
}

// checkConnParam rejects a ConnParam whose private data is longer than max.
func checkConnParam(op string, p *ConnParam, max int) error {
	if p == nil {
		return nil
	}
	return checkPrivateData(op, p.PrivateData, max)
}

// checkResolveAddrs rejects the addresses Resolve cannot pass on: remote
// needs an IP and a port; local is either the zero value or an IP of the
// remote's family with any port.
func checkResolveAddrs(local, remote netip.AddrPort) error {
	const op = "rdma_resolve_addr"
	if !remote.Addr().IsValid() || remote.Port() == 0 {
		return invalidArg(op, "remote address %v needs an IP and a port", remote)
	}
	if local == (netip.AddrPort{}) {
		return nil
	}
	if !local.Addr().IsValid() {
		return invalidArg(op, "local address %v has no IP", local)
	}
	if local.Addr().Unmap().Is4() != remote.Addr().Unmap().Is4() {
		return invalidArg(op, "local address %v and remote address %v differ in family", local, remote)
	}
	return nil
}

// checkListenAddr rejects a listen address without an IP; the unspecified
// address and port 0 are fine.
func checkListenAddr(local netip.AddrPort) error {
	if !local.Addr().IsValid() {
		return invalidArg("rdma_bind_addr", "listen address %v has no IP", local)
	}
	return nil
}
