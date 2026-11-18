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

// Dialer holds the options of the identifier Resolve creates, those of
// librdmacm's rdma_set_option, which it sets right after creating the
// identifier, before the address is bound and resolved. The zero Dialer sets
// none: Resolve is Dialer{}.Resolve.
//
// The kernel tells an unset TOS, AFOnly or AckTimeout from each of its
// values, so those are pointers, nil keeping the kernel's default, and
// new(v) setting v. ReuseAddr is off unless set, as in the kernel.
type Dialer struct {
	// TOS is the type of service of the connection's traffic, which the
	// route takes: on RoCE the traffic class of the path, from which the
	// priority mapping of the network device gives the service level; on
	// InfiniBand the QoS class of the path query with IPv4 addresses, and
	// nothing with IPv6 ones, whose path takes its traffic class from the
	// flow information of the source address, which Resolve leaves 0; on
	// iWARP the TOS of the connection's iWARP identifier, for its driver
	// to use. On RoCE and InfiniBand the peer's queue pair takes the
	// traffic class of the request. Unset, the device's default type of
	// service applies, 0 unless configured.
	TOS *uint8

	// ReuseAddr lets the identifier bind to a local address and port that
	// other identifiers with ReuseAddr hold, as SO_REUSEADDR does; it
	// counts when local names a port.
	ReuseAddr bool

	// AFOnly sets whether an identifier bound to a wildcard address keeps
	// its port to its own address family, as IPV6_V6ONLY does. Unset, the
	// kernel takes true for IPv4 and the sysctl net.ipv6.bindv6only for
	// IPv6.
	AFOnly *bool

	// AckTimeout is the local ACK timeout of the connection's queue pair,
	// 4.096 µs × 2^AckTimeout, from 0 to 31, 0 disabling the timer, which
	// ID.Connect takes the queue pair to RTS with. On RoCE a nonzero value
	// also sets the packet lifetime of the path, AckTimeout-1, from which
	// the peer derives its own timeout. Unset, the connection manager
	// derives the timeout from the path. iWARP has no such timer.
	AckTimeout *uint8
}

// ListenConfig holds the options of the identifier Listen creates, those of
// a Dialer, which it sets right after creating the identifier, before
// binding it. The zero ListenConfig sets none: Listen is
// ListenConfig{}.Listen.
//
// The kernel gives the identifier of a request none of the options of its
// listener. The Listener sets AckTimeout, the one with a step left on a
// request, on the identifier of each request before handing the request
// out; TOS, ReuseAddr and AFOnly concern the listening identifier alone.
type ListenConfig struct {
	// TOS is the type of service of the listening identifier, which on
	// iWARP passes to the device's listening endpoint, for its driver to
	// use. On RoCE and InfiniBand an accepted connection takes the
	// traffic class of the request instead.
	TOS *uint8

	// ReuseAddr lets the bind take a local address and port that
	// identifiers with ReuseAddr hold. A listener needs its port alone,
	// and the kernel clears the option when it listens, so such a Listen
	// fails in rdma_listen rather than in rdma_bind_addr.
	ReuseAddr bool

	// AFOnly sets whether a listener on a wildcard address takes the
	// requests of its own address family alone and leaves its port to the
	// other family's listeners, as IPV6_V6ONLY does. Unset, the kernel
	// takes true for IPv4 and the sysctl net.ipv6.bindv6only for IPv6.
	AFOnly *bool

	// AckTimeout is the local ACK timeout of the queue pair of each
	// accepted connection, as in Dialer, set on the request's identifier
	// before Request.Accept takes the queue pair to RTS.
	AckTimeout *uint8
}

// options are the identifier options of a Dialer or a ListenConfig.
type options struct {
	tos        *uint8
	reuseAddr  bool
	afOnly     *bool
	ackTimeout *uint8
}

func (d Dialer) options() options {
	return options{copyOf(d.TOS), d.ReuseAddr, copyOf(d.AFOnly), copyOf(d.AckTimeout)}
}

func (lc ListenConfig) options() options {
	return options{copyOf(lc.TOS), lc.ReuseAddr, copyOf(lc.AFOnly), copyOf(lc.AckTimeout)}
}

// copyOf returns a copy of *p, nil for nil, so that options keep the values
// a call took whatever the caller's variables hold afterwards.
func copyOf[T any](p *T) *T {
	if p == nil {
		return nil
	}
	return new(*p)
}

// maxAckTimeout is the largest ACK timeout, which the 5 bits of the queue
// pair's attribute hold.
const maxAckTimeout = 31

// check rejects the options no identifier takes.
func (o options) check() error {
	if o.ackTimeout != nil && *o.ackTimeout > maxAckTimeout {
		return invalidArg("rdma_set_option", "ACK timeout %d exceeds %d", *o.ackTimeout, maxAckTimeout)
	}
	return nil
}

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
//
// Reason is the status of the REJECTED event: the IB CM reject reason on
// InfiniBand and RoCE. iWARP has no reject reasons; there Reason is the
// negative errno the kernel reports the MPA reject with, -104 (ECONNRESET)
// on siw, and a reject without private data cannot be told from a refusal.
type RejectedError struct {
	PrivateData []byte // MaxRejectPrivateData bytes, zero padded; on iWARP as sent
	Reason      int    // 28 (consumer defined) for Request.Reject; negative on iWARP
}

func (e *RejectedError) Error() string {
	return "cm: connection rejected by peer (reason " + strconv.Itoa(e.Reason) + ")"
}

// Is reports whether target is syscall.ECONNREFUSED.
func (e *RejectedError) Is(target error) bool { return target == errConnRefused }

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
