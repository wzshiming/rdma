//go:build linux && cgo

package cm

/*
#cgo LDFLAGS: -lrdmacm
#include <errno.h>
#include <stdlib.h>
#include <rdma/rdma_cma.h>
*/
import "C"

import (
	"context"
	"encoding/binary"
	"errors"
	"net"
	"net/netip"
	"os"
	"sync"
	"sync/atomic"
	"syscall"
	"time"
	"unsafe"

	"github.com/wzshiming/rdma/verbs"
)

// defaultResolveTimeout bounds rdma_resolve_addr and rdma_resolve_route each
// when ctx carries no deadline.
const defaultResolveTimeout = 2 * time.Second

// ibCMRejConsumerDefined is the IB CM reject reason of an explicit
// rdma_reject (IB_CM_REJ_CONSUMER_DEFINED).
const ibCMRejConsumerDefined = 28

// opError reports a call that failed with errno set, as read by cgo into err.
func opError(op string, err error) error {
	errno, ok := err.(syscall.Errno)
	if !ok || errno == 0 {
		errno = syscall.EIO
	}
	return &verbs.OpError{Op: op, Err: errno}
}

func errClosed(op string) error { return &verbs.OpError{Op: op, Err: os.ErrClosed} }

// createID creates an event channel and an RDMA_PS_TCP identifier on it.
func createID() (*C.struct_rdma_event_channel, *C.struct_rdma_cm_id, error) {
	ch, err := C.rdma_create_event_channel()
	if ch == nil {
		return nil, nil, opError("rdma_create_event_channel", err)
	}
	var id *C.struct_rdma_cm_id
	if rc, err := C.rdma_create_id(ch, &id, nil, C.RDMA_PS_TCP); rc != 0 {
		C.rdma_destroy_event_channel(ch)
		return nil, nil, opError("rdma_create_id", err)
	}
	return ch, id, nil
}

// destroyID destroys an identifier and its channel. Every event this package
// reads is acknowledged at once and the kernel discards unread ones, so
// rdma_destroy_id has nothing to wait for.
func destroyID(id *C.struct_rdma_cm_id, ch *C.struct_rdma_event_channel) error {
	var err error
	if rc, e := C.rdma_destroy_id(id); rc != 0 {
		err = opError("rdma_destroy_id", e)
	}
	C.rdma_destroy_event_channel(ch)
	return err
}

// fillSockaddr stores ap in ss as a sockaddr_in or sockaddr_in6.
func fillSockaddr(ss *C.struct_sockaddr_storage, ap netip.AddrPort) {
	addr := ap.Addr().Unmap()
	if addr.Is4() {
		sa := (*syscall.RawSockaddrInet4)(unsafe.Pointer(ss))
		sa.Family = syscall.AF_INET
		binary.BigEndian.PutUint16((*[2]byte)(unsafe.Pointer(&sa.Port))[:], ap.Port())
		sa.Addr = addr.As4()
		return
	}
	sa := (*syscall.RawSockaddrInet6)(unsafe.Pointer(ss))
	sa.Family = syscall.AF_INET6
	binary.BigEndian.PutUint16((*[2]byte)(unsafe.Pointer(&sa.Port))[:], ap.Port())
	sa.Addr = addr.As16()
}

// addrPort reads a sockaddr_in or sockaddr_in6; other families give the zero
// value.
func addrPort(sa *C.struct_sockaddr) netip.AddrPort {
	switch sa.sa_family {
	case syscall.AF_INET:
		sa4 := (*syscall.RawSockaddrInet4)(unsafe.Pointer(sa))
		return netip.AddrPortFrom(netip.AddrFrom4(sa4.Addr), binary.BigEndian.Uint16((*[2]byte)(unsafe.Pointer(&sa4.Port))[:]))
	case syscall.AF_INET6:
		sa6 := (*syscall.RawSockaddrInet6)(unsafe.Pointer(sa))
		return netip.AddrPortFrom(netip.AddrFrom16(sa6.Addr), binary.BigEndian.Uint16((*[2]byte)(unsafe.Pointer(&sa6.Port))[:]))
	}
	return netip.AddrPort{}
}

// resolveTimeout is the rdma_cm timeout in milliseconds for one resolution
// step: the time left until the deadline of ctx, or defaultResolveTimeout.
func resolveTimeout(ctx context.Context) C.int {
	d := defaultResolveTimeout
	if deadline, ok := ctx.Deadline(); ok {
		d = time.Until(deadline)
	}
	return C.int(min(max(d.Milliseconds(), 1), 1<<31-1))
}

// cmEvent is a copy of a struct rdma_cm_event, taken before the event is
// acknowledged.
type cmEvent struct {
	typ                Event
	status             int
	errno              syscall.Errno // errno after rdma_get_cm_event, for a status of -1
	id                 *C.struct_rdma_cm_id
	privateData        []byte
	responderResources uint8
	initiatorDepth     uint8
}

// getEvent takes the next event off ch, copies and acknowledges it. It returns
// nil without an error when none is pending.
func getEvent(ch *C.struct_rdma_event_channel) (*cmEvent, error) {
	var cev *C.struct_rdma_cm_event
	var errno syscall.Errno
	for {
		rc, err := C.rdma_get_cm_event(ch, &cev)
		if rc == 0 {
			errno, _ = err.(syscall.Errno)
			break
		}
		if err == syscall.EINTR {
			continue
		}
		if err == syscall.EAGAIN {
			return nil, nil
		}
		return nil, opError("rdma_get_cm_event", err)
	}
	conn := (*C.struct_rdma_conn_param)(unsafe.Pointer(&cev.param))
	ev := &cmEvent{
		typ:                Event(cev.event),
		status:             int(cev.status),
		errno:              errno,
		id:                 cev.id,
		responderResources: uint8(conn.responder_resources),
		initiatorDepth:     uint8(conn.initiator_depth),
	}
	if conn.private_data_len > 0 {
		ev.privateData = C.GoBytes(conn.private_data, C.int(conn.private_data_len))
	}
	C.rdma_ack_cm_event(cev)
	return ev, nil
}

// err translates an event that ends a connection attempt into the error op
// reports; events that do not end it give nil.
func (ev *cmEvent) err(op string) error {
	switch ev.typ {
	case EventRejected:
		if ev.status == ibCMRejConsumerDefined {
			return &RejectedError{PrivateData: ev.privateData, Reason: ev.status}
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
// errno values, librdmacm -1 with errno set; def covers the rest.
func (ev *cmEvent) statusErrno(def syscall.Errno) syscall.Errno {
	switch {
	case ev.status < -1:
		return syscall.Errno(-ev.status)
	case ev.status == -1 && ev.errno != 0:
		return ev.errno
	}
	return def
}

// ID is a communication identifier (struct rdma_cm_id) with an event channel
// of its own.
type ID struct {
	id     *C.struct_rdma_cm_id
	ch     *C.struct_rdma_event_channel
	w      *fdWaiter
	dev    *verbs.Context
	local  netip.AddrPort
	remote netip.AddrPort

	disconnected atomic.Bool // a DISCONNECTED event was read

	mu     sync.Mutex // serialises the rdma_cm calls below with Close
	closed bool
	qp     *verbs.QP
}

// newID wraps id, whose address and device are known, with its channel ch.
func newID(id *C.struct_rdma_cm_id, ch *C.struct_rdma_event_channel) (*ID, error) {
	w, err := newFDWaiter(int(ch.fd))
	if err != nil {
		destroyID(id, ch)
		return nil, err
	}
	return &ID{
		id:     id,
		ch:     ch,
		w:      w,
		dev:    verbs.ContextFromPointer(unsafe.Pointer(id.verbs)),
		local:  addrPort(C.rdma_get_local_addr(id)),
		remote: addrPort(C.rdma_get_peer_addr(id)),
	}, nil
}

// Resolve resolves the address and route to remote and returns an identifier
// bound to the device and port that reach it. A zero local lets rdma_cm pick
// the local address; otherwise its IP selects the device and its port, if
// not 0, the local port. Both must be IP addresses: host names are the
// caller's business. The timeout of each resolution step comes from the
// deadline of ctx, or is 2 seconds.
func Resolve(ctx context.Context, local, remote netip.AddrPort) (*ID, error) {
	if err := checkResolveAddrs(local, remote); err != nil {
		return nil, err
	}
	if err := ctx.Err(); err != nil {
		return nil, err
	}
	ch, id, err := createID()
	if err != nil {
		return nil, err
	}
	w, err := newFDWaiter(int(ch.fd))
	if err != nil {
		destroyID(id, ch)
		return nil, err
	}
	r := &ID{id: id, ch: ch, w: w}
	var src *C.struct_sockaddr
	var srcStorage, dstStorage C.struct_sockaddr_storage
	if local.Addr().IsValid() {
		fillSockaddr(&srcStorage, local)
		src = (*C.struct_sockaddr)(unsafe.Pointer(&srcStorage))
	}
	fillSockaddr(&dstStorage, remote)
	dst := (*C.struct_sockaddr)(unsafe.Pointer(&dstStorage))
	if rc, err := C.rdma_resolve_addr(id, src, dst, resolveTimeout(ctx)); rc != 0 {
		r.Close()
		return nil, opError("rdma_resolve_addr", err)
	}
	if _, err := r.awaitEvent(ctx, "rdma_resolve_addr", EventAddrResolved); err != nil {
		r.Close()
		return nil, err
	}
	if rc, err := C.rdma_resolve_route(id, resolveTimeout(ctx)); rc != 0 {
		r.Close()
		return nil, opError("rdma_resolve_route", err)
	}
	if _, err := r.awaitEvent(ctx, "rdma_resolve_route", EventRouteResolved); err != nil {
		r.Close()
		return nil, err
	}
	r.dev = verbs.ContextFromPointer(unsafe.Pointer(id.verbs))
	r.local = addrPort(C.rdma_get_local_addr(id))
	r.remote = addrPort(C.rdma_get_peer_addr(id))
	return r, nil
}

// Device returns the device the connection uses. rdma_cm keeps it open for
// the life of the process; Close on it releases nothing.
func (id *ID) Device() *verbs.Context { return id.dev }

// Port returns the device port the connection uses.
func (id *ID) Port() uint8 { return uint8(id.id.port_num) }

// LocalAddr returns the local address of the connection.
func (id *ID) LocalAddr() netip.AddrPort { return id.local }

// RemoteAddr returns the address of the peer.
func (id *ID) RemoteAddr() netip.AddrPort { return id.remote }

// CreateQP creates the queue pair of the connection (rdma_create_qp) and
// stores the granted capacities in attr.Cap. pd must belong to Device();
// attr.SendCQ and attr.RecvCQ are required and must outlive the ID, which
// destroys the queue pair when it closes.
func (id *ID) CreateQP(pd *verbs.PD, attr *verbs.QPInitAttr) (*verbs.QP, error) {
	if attr.SendCQ == nil || attr.RecvCQ == nil {
		return nil, invalidArg("rdma_create_qp", "send and receive completion queues are required")
	}
	id.mu.Lock()
	defer id.mu.Unlock()
	if id.closed {
		return nil, errClosed("rdma_create_qp")
	}
	ia := C.struct_ibv_qp_init_attr{
		send_cq: (*C.struct_ibv_cq)(attr.SendCQ.Pointer()),
		recv_cq: (*C.struct_ibv_cq)(attr.RecvCQ.Pointer()),
		cap: C.struct_ibv_qp_cap{
			max_send_wr:     C.uint32_t(attr.Cap.MaxSendWR),
			max_recv_wr:     C.uint32_t(attr.Cap.MaxRecvWR),
			max_send_sge:    C.uint32_t(attr.Cap.MaxSendSGE),
			max_recv_sge:    C.uint32_t(attr.Cap.MaxRecvSGE),
			max_inline_data: C.uint32_t(attr.Cap.MaxInlineData),
		},
		qp_type: uint32(attr.Type),
	}
	if attr.SignalAll {
		ia.sq_sig_all = 1
	}
	var cpd *C.struct_ibv_pd
	if pd != nil {
		cpd = (*C.struct_ibv_pd)(pd.Pointer())
	}
	if rc, err := C.rdma_create_qp(id.id, cpd, &ia); rc != 0 {
		return nil, opError("rdma_create_qp", err)
	}
	attr.Cap = verbs.QPCap{
		MaxSendWR:     uint32(ia.cap.max_send_wr),
		MaxRecvWR:     uint32(ia.cap.max_recv_wr),
		MaxSendSGE:    uint32(ia.cap.max_send_sge),
		MaxRecvSGE:    uint32(ia.cap.max_recv_sge),
		MaxInlineData: uint32(ia.cap.max_inline_data),
	}
	id.qp = verbs.QPFromPointer(unsafe.Pointer(id.id.qp))
	return id.qp, nil
}

// QP returns the queue pair created by CreateQP, or nil.
func (id *ID) QP() *verbs.QP {
	id.mu.Lock()
	defer id.mu.Unlock()
	return id.qp
}

// connParam fills a struct rdma_conn_param from p; the returned function
// frees the private data copy.
func connParam(p ConnParam) (*C.struct_rdma_conn_param, func()) {
	cp := &C.struct_rdma_conn_param{
		responder_resources: C.uint8_t(p.ResponderResources),
		initiator_depth:     C.uint8_t(p.InitiatorDepth),
		retry_count:         C.uint8_t(p.RetryCount),
		rnr_retry_count:     C.uint8_t(p.RNRRetryCount),
	}
	if len(p.PrivateData) > 0 {
		cp.private_data = C.CBytes(p.PrivateData)
		cp.private_data_len = C.uint8_t(len(p.PrivateData))
	}
	return cp, func() { C.free(cp.private_data) }
}

// Connect connects to the peer with the queue pair of CreateQP and waits until
// the connection is established; param may be nil for the defaults. It returns
// the peer's private data and the negotiated parameters, a *RejectedError when
// the peer rejects explicitly, a *verbs.OpError with syscall.ECONNREFUSED when
// nobody listens, syscall.ETIMEDOUT or syscall.EHOSTUNREACH when the peer is
// out of reach, and ctx.Err() when ctx ends first, after which the only
// sensible call on the ID is Close.
func (id *ID) Connect(ctx context.Context, param *ConnParam) (*ConnInfo, error) {
	ev, err := id.establish(ctx, "rdma_connect", param, MaxConnectPrivateData, func(cp *C.struct_rdma_conn_param) (C.int, error) {
		rc, err := C.rdma_connect(id.id, cp)
		return rc, err
	})
	if err != nil {
		return nil, err
	}
	return &ConnInfo{PrivateData: ev.privateData, ResponderResources: ev.responderResources, InitiatorDepth: ev.initiatorDepth}, nil
}

// establish runs call, an rdma_connect or rdma_accept named op, with the
// parameters of param, whose private data may have max bytes, and waits for
// the ESTABLISHED event.
func (id *ID) establish(ctx context.Context, op string, param *ConnParam, max int, call func(*C.struct_rdma_conn_param) (C.int, error)) (*cmEvent, error) {
	if err := checkConnParam(op, param, max); err != nil {
		return nil, err
	}
	id.mu.Lock()
	if id.closed {
		id.mu.Unlock()
		return nil, errClosed(op)
	}
	if id.qp == nil {
		id.mu.Unlock()
		return nil, invalidArg(op, "no queue pair, call CreateQP first")
	}
	cp, free := connParam(param.withDefaults())
	rc, err := call(cp)
	free()
	id.mu.Unlock()
	if rc != 0 {
		return nil, opError(op, err)
	}
	return id.awaitEvent(ctx, op, EventEstablished)
}

// awaitEvent reads events until want arrives and returns it; an event that
// ends the attempt returns its error instead, other events are skipped.
func (id *ID) awaitEvent(ctx context.Context, op string, want Event) (*cmEvent, error) {
	var got *cmEvent
	err := id.w.wait(ctx, op, func() (bool, error) {
		for {
			ev, err := id.getEvent()
			if err != nil || ev == nil {
				return false, err
			}
			if ev.typ == want {
				got = ev
				return true, nil
			}
			if err := ev.err(op); err != nil {
				return false, err
			}
		}
	})
	if err != nil {
		return nil, err
	}
	return got, nil
}

// getEvent reads and acknowledges the next event of the ID, noting a
// disconnect.
func (id *ID) getEvent() (*cmEvent, error) {
	ev, err := getEvent(id.ch)
	if ev != nil && ev.typ == EventDisconnected {
		id.disconnected.Store(true)
	}
	return ev, err
}

// Disconnect disconnects from the peer (rdma_disconnect) and waits for the
// DISCONNECTED event, unless Wait or an earlier Disconnect already consumed
// the peer's. The queue pair moves to the error state.
func (id *ID) Disconnect(ctx context.Context) error {
	id.mu.Lock()
	if id.closed {
		id.mu.Unlock()
		return errClosed("rdma_disconnect")
	}
	rc, err := C.rdma_disconnect(id.id)
	id.mu.Unlock()
	if rc != 0 {
		return opError("rdma_disconnect", err)
	}
	return id.w.wait(ctx, "rdma_disconnect", func() (bool, error) {
		for {
			if id.disconnected.Load() {
				return true, nil
			}
			ev, err := id.getEvent()
			if err != nil || ev == nil {
				return false, err
			}
			if ev.typ == EventDeviceRemoval {
				return false, ev.err("rdma_disconnect")
			}
		}
	})
}

// Wait waits for the next event of the connection, such as EventDisconnected
// when the peer disconnects, EventTimewaitExit after a disconnect,
// EventAddrChange or EventDeviceRemoval. It returns ctx.Err() when ctx ends
// first and an error wrapping os.ErrClosed once the ID is closed.
func (id *ID) Wait(ctx context.Context) (Event, error) {
	id.mu.Lock()
	closed := id.closed
	id.mu.Unlock()
	if closed {
		return 0, errClosed("rdma_get_cm_event")
	}
	var got Event
	err := id.w.wait(ctx, "rdma_get_cm_event", func() (bool, error) {
		ev, err := id.getEvent()
		if err != nil || ev == nil {
			return false, err
		}
		got = ev.typ
		return true, nil
	})
	return got, err
}

// Close destroys the queue pair and the identifier and wakes a pending wait
// with an error. It never blocks on pending events; a second Close is a no-op.
func (id *ID) Close() error {
	id.mu.Lock()
	defer id.mu.Unlock()
	if id.closed {
		return nil
	}
	id.closed = true
	id.w.close()
	id.qp = nil
	if id.id.qp != nil {
		C.rdma_destroy_qp(id.id)
	}
	return destroyID(id.id, id.ch)
}

// Listener listens for connection requests on an address.
type Listener struct {
	id   *C.struct_rdma_cm_id
	ch   *C.struct_rdma_event_channel
	w    *fdWaiter
	addr netip.AddrPort

	mu     sync.Mutex
	closed bool
}

// Listen binds local, whose IP picks the device and may be unspecified, and
// listens for connection requests; port 0 picks a free port. backlog bounds
// the requests waiting to be accepted, 0 meaning the kernel's maximum.
func Listen(local netip.AddrPort, backlog int) (*Listener, error) {
	if err := checkListenAddr(local); err != nil {
		return nil, err
	}
	ch, id, err := createID()
	if err != nil {
		return nil, err
	}
	var ss C.struct_sockaddr_storage
	fillSockaddr(&ss, local)
	if rc, err := C.rdma_bind_addr(id, (*C.struct_sockaddr)(unsafe.Pointer(&ss))); rc != 0 {
		destroyID(id, ch)
		return nil, opError("rdma_bind_addr", err)
	}
	if rc, err := C.rdma_listen(id, C.int(backlog)); rc != 0 {
		destroyID(id, ch)
		return nil, opError("rdma_listen", err)
	}
	w, err := newFDWaiter(int(ch.fd))
	if err != nil {
		destroyID(id, ch)
		return nil, err
	}
	return &Listener{id: id, ch: ch, w: w, addr: addrPort(C.rdma_get_local_addr(id))}, nil
}

// Addr returns the bound address, with the port actually chosen for port 0.
func (l *Listener) Addr() netip.AddrPort { return l.addr }

// Accept waits for the next connection request and returns it with its
// private data and parameters; the request's ID is on an event channel of its
// own. It returns ctx.Err() when ctx ends first and an error wrapping
// net.ErrClosed once the Listener is closed.
func (l *Listener) Accept(ctx context.Context) (*Request, error) {
	l.mu.Lock()
	closed := l.closed
	l.mu.Unlock()
	if closed {
		return nil, &verbs.OpError{Op: "rdma_get_cm_event", Err: net.ErrClosed}
	}
	var req *Request
	err := l.w.wait(ctx, "rdma_get_cm_event", func() (bool, error) {
		for {
			ev, err := getEvent(l.ch)
			if err != nil || ev == nil {
				return false, err
			}
			switch ev.typ {
			case EventConnectRequest:
				id, err := migrate(ev.id)
				if err != nil {
					return false, err
				}
				req = &Request{ID: id, PrivateData: ev.privateData, ResponderResources: ev.responderResources, InitiatorDepth: ev.initiatorDepth}
				return true, nil
			case EventDeviceRemoval:
				return false, ev.err("rdma_get_cm_event")
			}
		}
	})
	if err != nil {
		if errors.Is(err, os.ErrClosed) {
			return nil, &verbs.OpError{Op: "rdma_get_cm_event", Err: net.ErrClosed}
		}
		return nil, err
	}
	return req, nil
}

// migrate moves an incoming identifier from the listener's event channel to
// one of its own.
func migrate(id *C.struct_rdma_cm_id) (*ID, error) {
	ch, err := C.rdma_create_event_channel()
	if ch == nil {
		C.rdma_destroy_id(id)
		return nil, opError("rdma_create_event_channel", err)
	}
	if rc, err := C.rdma_migrate_id(id, ch); rc != 0 {
		C.rdma_destroy_id(id)
		C.rdma_destroy_event_channel(ch)
		return nil, opError("rdma_migrate_id", err)
	}
	return newID(id, ch)
}

// Close stops listening and wakes a pending Accept with net.ErrClosed.
// Requests not yet accepted are rejected by the kernel. A second Close is a
// no-op.
func (l *Listener) Close() error {
	l.mu.Lock()
	defer l.mu.Unlock()
	if l.closed {
		return nil
	}
	l.closed = true
	l.w.close()
	return destroyID(l.id, l.ch)
}

// Accept accepts the request with the queue pair created on its ID and waits
// until the connection is established; param may be nil for the defaults.
// The private data goes to the peer's Connect.
func (r *Request) Accept(ctx context.Context, param *ConnParam) error {
	id := r.ID
	_, err := id.establish(ctx, "rdma_accept", param, MaxAcceptPrivateData, func(cp *C.struct_rdma_conn_param) (C.int, error) {
		rc, err := C.rdma_accept(id.id, cp)
		return rc, err
	})
	return err
}

// Reject rejects the request, handing privateData to the peer's Connect as a
// *RejectedError, and closes the request's ID.
func (r *Request) Reject(privateData []byte) error {
	if err := checkPrivateData("rdma_reject", privateData, MaxRejectPrivateData); err != nil {
		return err
	}
	id := r.ID
	id.mu.Lock()
	if id.closed {
		id.mu.Unlock()
		return errClosed("rdma_reject")
	}
	var data unsafe.Pointer
	if len(privateData) > 0 {
		data = C.CBytes(privateData)
	}
	rc, err := C.rdma_reject(id.id, data, C.uint8_t(len(privateData)))
	C.free(data)
	id.mu.Unlock()
	if rc != 0 {
		return opError("rdma_reject", err)
	}
	return id.Close()
}
