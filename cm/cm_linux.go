//go:build linux && !android && (amd64 || arm64 || loong64 || ppc64le || riscv64 || s390x)

package cm

import (
	"bytes"
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

	"github.com/wzshiming/rdma/internal/dynlib"
	"github.com/wzshiming/rdma/internal/rdmaabi"
	"github.com/wzshiming/rdma/verbs"
)

// defaultResolveTimeout bounds rdma_resolve_addr and rdma_resolve_route each
// when ctx carries no deadline.
const defaultResolveTimeout = 2 * time.Second

// ibCMRejConsumerDefined is the IB CM reject reason of an explicit
// rdma_reject (IB_CM_REJ_CONSUMER_DEFINED).
const ibCMRejConsumerDefined = 28

// rdmaPSTCP is the port space of every identifier (RDMA_PS_TCP).
const rdmaPSTCP = 0x0106

// createID creates an event channel and an RDMA_PS_TCP identifier on it.
func createID(lib *funcs) (*rdmaabi.RdmaEventChannel, *rdmaabi.RdmaCmId, error) {
	ch, errno := lib.createEventChannel.Ptr(0, 0, 0, 0, 0, 0)
	if ch == 0 {
		return nil, nil, opError("rdma_create_event_channel", errno)
	}
	var id uintptr // struct rdma_cm_id *, stored by the call
	if rc, errno := lib.createID.Int(ch, uintptr(unsafe.Pointer(&id)), 0, rdmaPSTCP, 0, 0); rc != 0 {
		lib.destroyEventChannel.Int(ch, 0, 0, 0, 0, 0)
		return nil, nil, opError("rdma_create_id", errno)
	}
	return dynlib.Pointer[rdmaabi.RdmaEventChannel](ch), dynlib.Pointer[rdmaabi.RdmaCmId](id), nil
}

// destroyID destroys an identifier and its channel. Every event this package
// reads is acknowledged at once and the kernel discards unread ones, so
// rdma_destroy_id has nothing to wait for.
func destroyID(lib *funcs, id *rdmaabi.RdmaCmId, ch *rdmaabi.RdmaEventChannel) error {
	var err error
	if rc, errno := lib.destroyID.Int(caddr(id), 0, 0, 0, 0, 0); rc != 0 {
		err = opError("rdma_destroy_id", errno)
	}
	lib.destroyEventChannel.Int(caddr(ch), 0, 0, 0, 0, 0)
	return err
}

// sockaddrIn and sockaddrIn6 are struct sockaddr_in and struct sockaddr_in6:
// the family an sa_family_t in the host's byte order, the port two bytes in
// network order whatever the host's, then the address; sin6_flowinfo and
// sin6_scope_id are neither set nor read.
type sockaddrIn struct {
	family uint16
	port   [2]byte
	addr   [4]byte
	_      [8]byte
}

type sockaddrIn6 struct {
	family   uint16
	port     [2]byte
	flowinfo uint32
	addr     [16]byte
	scopeID  uint32
}

// sockaddrStorage is struct sockaddr_storage, which holds either of them and
// is aligned like it: the type of the two addresses in rdma_cm_id.route.addr
// and of the address rdma_bind_addr and rdma_resolve_addr read.
type sockaddrStorage struct {
	family uint16
	_      [6]byte
	_      [15]uint64
}

// set stores ap in ss; a v4-mapped address becomes an IPv4 sockaddr.
func (ss *sockaddrStorage) set(ap netip.AddrPort) {
	*ss = sockaddrStorage{}
	var port [2]byte
	binary.BigEndian.PutUint16(port[:], ap.Port())
	addr := ap.Addr().Unmap()
	if addr.Is4() {
		*(*sockaddrIn)(unsafe.Pointer(ss)) = sockaddrIn{family: syscall.AF_INET, port: port, addr: addr.As4()}
		return
	}
	*(*sockaddrIn6)(unsafe.Pointer(ss)) = sockaddrIn6{family: syscall.AF_INET6, port: port, addr: addr.As16()}
}

// addrPort reads an IPv4 or IPv6 sockaddr; other families give the zero
// value.
func (ss *sockaddrStorage) addrPort() netip.AddrPort {
	switch ss.family {
	case syscall.AF_INET:
		sin := (*sockaddrIn)(unsafe.Pointer(ss))
		return netip.AddrPortFrom(netip.AddrFrom4(sin.addr), binary.BigEndian.Uint16(sin.port[:]))
	case syscall.AF_INET6:
		sin6 := (*sockaddrIn6)(unsafe.Pointer(ss))
		return netip.AddrPortFrom(netip.AddrFrom16(sin6.addr), binary.BigEndian.Uint16(sin6.port[:]))
	}
	return netip.AddrPort{}
}

// addrs reads the local and peer addresses of id from its route, as the
// header inlines rdma_get_local_addr and rdma_get_peer_addr do.
func addrs(id *rdmaabi.RdmaCmId) (local, remote netip.AddrPort) {
	local = (*sockaddrStorage)(unsafe.Pointer(&id.Route.Addr.Src_storage)).addrPort()
	remote = (*sockaddrStorage)(unsafe.Pointer(&id.Route.Addr.Dst_storage)).addrPort()
	return local, remote
}

// resolveTimeout is the rdma_cm timeout in milliseconds for one resolution
// step: the time left until the deadline of ctx, or defaultResolveTimeout.
func resolveTimeout(ctx context.Context) uintptr {
	d := defaultResolveTimeout
	if deadline, ok := ctx.Deadline(); ok {
		d = time.Until(deadline)
	}
	return uintptr(min(max(d.Milliseconds(), 1), 1<<31-1))
}

// cmEvent is a copy of a struct rdma_cm_event, taken before the event is
// acknowledged.
type cmEvent struct {
	typ                Event
	status             int
	id                 *rdmaabi.RdmaCmId
	privateData        []byte
	responderResources uint8
	initiatorDepth     uint8
}

// getEvent takes the next event off ch, copies and acknowledges it. It returns
// nil without an error when none is pending.
func getEvent(lib *funcs, ch *rdmaabi.RdmaEventChannel) (*cmEvent, error) {
	var cev uintptr // struct rdma_cm_event *, stored by the call
	for {
		rc, errno := lib.getCMEvent.Int(caddr(ch), uintptr(unsafe.Pointer(&cev)), 0, 0, 0, 0)
		if rc == 0 {
			break
		}
		if errno == syscall.EINTR {
			continue
		}
		if errno == syscall.EAGAIN {
			return nil, nil
		}
		return nil, opError("rdma_get_cm_event", errno)
	}
	ev := eventFromC(dynlib.Pointer[rdmaabi.RdmaCmEvent](cev))
	lib.ackCMEvent.Int(cev, 0, 0, 0, 0, 0)
	return ev, nil
}

// eventFromC copies the fields of a struct rdma_cm_event this package reads,
// the private data included, so that the event can be acknowledged.
func eventFromC(cev *rdmaabi.RdmaCmEvent) *cmEvent {
	conn := &cev.Param.Conn
	ev := &cmEvent{
		typ:                Event(cev.Event),
		status:             int(cev.Status),
		id:                 dynlib.Pointer[rdmaabi.RdmaCmId](uintptr(cev.Id)),
		responderResources: conn.Responder_resources,
		initiatorDepth:     conn.Initiator_depth,
	}
	if conn.Private_data_len > 0 {
		ev.privateData = bytes.Clone(unsafe.Slice(dynlib.Pointer[byte](uintptr(conn.Private_data)), int(conn.Private_data_len)))
	}
	return ev
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
// errno values; def covers the rest, among them the -1 of a failure inside
// librdmacm, whose errno a call that returned 0 does not hand back.
func (ev *cmEvent) statusErrno(def syscall.Errno) syscall.Errno {
	if ev.status < -1 {
		return syscall.Errno(-ev.status)
	}
	return def
}

// ID is a communication identifier (struct rdma_cm_id) with an event channel
// of its own.
type ID struct {
	lib    *funcs
	id     *rdmaabi.RdmaCmId
	ch     *rdmaabi.RdmaEventChannel
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
func newID(lib *funcs, id *rdmaabi.RdmaCmId, ch *rdmaabi.RdmaEventChannel) (*ID, error) {
	w, err := newFDWaiter(int(ch.Fd))
	if err != nil {
		destroyID(lib, id, ch)
		return nil, err
	}
	r := &ID{lib: lib, id: id, ch: ch, w: w}
	r.readRoute()
	return r, nil
}

// readRoute records the device and the addresses of an identifier whose
// route is known.
func (id *ID) readRoute() {
	id.dev = verbs.ContextFromPointer(cpointer(id.id.Verbs))
	id.local, id.remote = addrs(id.id)
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
	lib, err := load("rdma_resolve_addr")
	if err != nil {
		return nil, err
	}
	ch, id, err := createID(lib)
	if err != nil {
		return nil, err
	}
	w, err := newFDWaiter(int(ch.Fd))
	if err != nil {
		destroyID(lib, id, ch)
		return nil, err
	}
	r := &ID{lib: lib, id: id, ch: ch, w: w}
	var src *sockaddrStorage
	var srcStorage, dstStorage sockaddrStorage
	if local.Addr().IsValid() {
		srcStorage.set(local)
		src = &srcStorage
	}
	dstStorage.set(remote)
	if rc, errno := lib.resolveAddr.Int(caddr(id), uintptr(unsafe.Pointer(src)), uintptr(unsafe.Pointer(&dstStorage)), resolveTimeout(ctx), 0, 0); rc != 0 {
		r.Close()
		return nil, opError("rdma_resolve_addr", errno)
	}
	if _, err := r.awaitEvent(ctx, "rdma_resolve_addr", EventAddrResolved); err != nil {
		r.Close()
		return nil, err
	}
	if rc, errno := lib.resolveRoute.Int(caddr(id), resolveTimeout(ctx), 0, 0, 0, 0); rc != 0 {
		r.Close()
		return nil, opError("rdma_resolve_route", errno)
	}
	if _, err := r.awaitEvent(ctx, "rdma_resolve_route", EventRouteResolved); err != nil {
		r.Close()
		return nil, err
	}
	r.readRoute()
	return r, nil
}

// Device returns the device the connection uses. rdma_cm keeps it open for
// the life of the process; Close on it releases nothing.
func (id *ID) Device() *verbs.Context { return id.dev }

// Port returns the device port the connection uses.
func (id *ID) Port() uint8 { return id.id.Port_num }

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
	ia := rdmaabi.IbvQpInitAttr{
		Send_cq: uint64(uintptr(attr.SendCQ.Pointer())),
		Recv_cq: uint64(uintptr(attr.RecvCQ.Pointer())),
		Cap: rdmaabi.IbvQpCap{
			Max_send_wr:     attr.Cap.MaxSendWR,
			Max_recv_wr:     attr.Cap.MaxRecvWR,
			Max_send_sge:    attr.Cap.MaxSendSGE,
			Max_recv_sge:    attr.Cap.MaxRecvSGE,
			Max_inline_data: attr.Cap.MaxInlineData,
		},
		Qp_type: uint32(attr.Type),
	}
	if attr.SignalAll {
		ia.Sq_sig_all = 1
	}
	var cpd unsafe.Pointer
	if pd != nil {
		cpd = pd.Pointer()
	}
	if rc, errno := id.lib.createQP.Int(caddr(id.id), uintptr(cpd), uintptr(unsafe.Pointer(&ia)), 0, 0, 0); rc != 0 {
		return nil, opError("rdma_create_qp", errno)
	}
	attr.Cap = verbs.QPCap{
		MaxSendWR:     ia.Cap.Max_send_wr,
		MaxRecvWR:     ia.Cap.Max_recv_wr,
		MaxSendSGE:    ia.Cap.Max_send_sge,
		MaxRecvSGE:    ia.Cap.Max_recv_sge,
		MaxInlineData: ia.Cap.Max_inline_data,
	}
	id.qp = verbs.QPFromPointer(cpointer(id.id.Qp))
	return id.qp, nil
}

// QP returns the queue pair created by CreateQP, or nil.
func (id *ID) QP() *verbs.QP {
	id.mu.Lock()
	defer id.mu.Unlock()
	return id.qp
}

// connParam is the memory of one rdma_connect or rdma_accept: the struct
// rdma_conn_param and the copy of the private data it points to, in one
// heap object that the call keeps alive.
type connParam struct {
	c    rdmaabi.RdmaConnParam
	data [MaxAcceptPrivateData]byte
}

// newConnParam fills a connParam from p, whose private data fits data.
func newConnParam(p ConnParam) *connParam {
	cp := &connParam{c: rdmaabi.RdmaConnParam{
		Responder_resources: p.ResponderResources,
		Initiator_depth:     p.InitiatorDepth,
		Retry_count:         p.RetryCount,
		Rnr_retry_count:     p.RNRRetryCount,
	}}
	if n := copy(cp.data[:], p.PrivateData); n > 0 {
		cp.c.Private_data = uint64(uintptr(unsafe.Pointer(&cp.data)))
		cp.c.Private_data_len = uint8(n)
	}
	return cp
}

// Connect connects to the peer with the queue pair of CreateQP and waits until
// the connection is established; param may be nil for the defaults. It returns
// the peer's private data and the negotiated parameters, a *RejectedError when
// the peer rejects explicitly, a *verbs.OpError with syscall.ECONNREFUSED when
// nobody listens, syscall.ETIMEDOUT or syscall.EHOSTUNREACH when the peer is
// out of reach, and ctx.Err() when ctx ends first, after which the only
// sensible call on the ID is Close.
func (id *ID) Connect(ctx context.Context, param *ConnParam) (*ConnInfo, error) {
	ev, err := id.establish(ctx, "rdma_connect", param, MaxConnectPrivateData, func(cp *rdmaabi.RdmaConnParam) (int32, syscall.Errno) {
		return id.lib.connect.Int(caddr(id.id), uintptr(unsafe.Pointer(cp)), 0, 0, 0, 0)
	})
	if err != nil {
		return nil, err
	}
	return &ConnInfo{PrivateData: ev.privateData, ResponderResources: ev.responderResources, InitiatorDepth: ev.initiatorDepth}, nil
}

// establish runs call, an rdma_connect or rdma_accept named op, with the
// parameters of param, whose private data may have max bytes, and waits for
// the ESTABLISHED event.
func (id *ID) establish(ctx context.Context, op string, param *ConnParam, max int, call func(*rdmaabi.RdmaConnParam) (int32, syscall.Errno)) (*cmEvent, error) {
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
	cp := newConnParam(param.withDefaults())
	rc, errno := call(&cp.c)
	id.mu.Unlock()
	if rc != 0 {
		return nil, opError(op, errno)
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
	ev, err := getEvent(id.lib, id.ch)
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
	rc, errno := id.lib.disconnect.Int(caddr(id.id), 0, 0, 0, 0, 0)
	id.mu.Unlock()
	if rc != 0 {
		return opError("rdma_disconnect", errno)
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
	if id.id.Qp != 0 {
		id.lib.destroyQP.Int(caddr(id.id), 0, 0, 0, 0, 0)
	}
	return destroyID(id.lib, id.id, id.ch)
}

// Listener listens for connection requests on an address.
type Listener struct {
	lib  *funcs
	id   *rdmaabi.RdmaCmId
	ch   *rdmaabi.RdmaEventChannel
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
	lib, err := load("rdma_listen")
	if err != nil {
		return nil, err
	}
	ch, id, err := createID(lib)
	if err != nil {
		return nil, err
	}
	var ss sockaddrStorage
	ss.set(local)
	if rc, errno := lib.bindAddr.Int(caddr(id), uintptr(unsafe.Pointer(&ss)), 0, 0, 0, 0); rc != 0 {
		destroyID(lib, id, ch)
		return nil, opError("rdma_bind_addr", errno)
	}
	if rc, errno := lib.listen.Int(caddr(id), uintptr(backlog), 0, 0, 0, 0); rc != 0 {
		destroyID(lib, id, ch)
		return nil, opError("rdma_listen", errno)
	}
	w, err := newFDWaiter(int(ch.Fd))
	if err != nil {
		destroyID(lib, id, ch)
		return nil, err
	}
	bound, _ := addrs(id)
	return &Listener{lib: lib, id: id, ch: ch, w: w, addr: bound}, nil
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
			ev, err := getEvent(l.lib, l.ch)
			if err != nil || ev == nil {
				return false, err
			}
			switch ev.typ {
			case EventConnectRequest:
				id, err := migrate(l.lib, ev.id)
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
func migrate(lib *funcs, id *rdmaabi.RdmaCmId) (*ID, error) {
	ch, errno := lib.createEventChannel.Ptr(0, 0, 0, 0, 0, 0)
	if ch == 0 {
		lib.destroyID.Int(caddr(id), 0, 0, 0, 0, 0)
		return nil, opError("rdma_create_event_channel", errno)
	}
	if rc, errno := lib.migrateID.Int(caddr(id), ch, 0, 0, 0, 0); rc != 0 {
		lib.destroyID.Int(caddr(id), 0, 0, 0, 0, 0)
		lib.destroyEventChannel.Int(ch, 0, 0, 0, 0, 0)
		return nil, opError("rdma_migrate_id", errno)
	}
	return newID(lib, id, dynlib.Pointer[rdmaabi.RdmaEventChannel](ch))
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
	return destroyID(l.lib, l.id, l.ch)
}

// Accept accepts the request with the queue pair created on its ID and waits
// until the connection is established; param may be nil for the defaults.
// The private data goes to the peer's Connect.
func (r *Request) Accept(ctx context.Context, param *ConnParam) error {
	id := r.ID
	_, err := id.establish(ctx, "rdma_accept", param, MaxAcceptPrivateData, func(cp *rdmaabi.RdmaConnParam) (int32, syscall.Errno) {
		return id.lib.accept.Int(caddr(id.id), uintptr(unsafe.Pointer(cp)), 0, 0, 0, 0)
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
	var data *byte
	if len(privateData) > 0 {
		data = &privateData[0]
	}
	rc, errno := id.lib.reject.Int(caddr(id.id), uintptr(unsafe.Pointer(data)), uintptr(len(privateData)), 0, 0, 0)
	id.mu.Unlock()
	if rc != 0 {
		return opError("rdma_reject", errno)
	}
	return id.Close()
}
