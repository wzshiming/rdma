//go:build linux && !android && (amd64 || arm64 || loong64 || ppc64le || riscv64 || s390x)

package cm

import (
	"context"
	"errors"
	"net"
	"net/netip"
	"os"
	"sync"
	"sync/atomic"
	"syscall"
	"time"

	"github.com/wzshiming/rdma/internal/ucma"
	"github.com/wzshiming/rdma/verbs"
)

// defaultResolveTimeout bounds rdma_resolve_addr and rdma_resolve_route each
// when ctx carries no deadline.
const defaultResolveTimeout = 2 * time.Second

// lastUID numbers the identifiers for the kernel, whose events of an
// identifier carry its number: never 0, which the kernel's events of an
// identifier not yet accepted carry, and never a Go pointer.
var lastUID atomic.Uint64

func newUID() uint64 { return lastUID.Add(1) }

// opError reports an operation that failed with err, an errno of the
// kernel's device or of verbs.
func opError(op string, err error) error { return &verbs.OpError{Op: op, Err: err} }

func errClosed(op string) error { return &verbs.OpError{Op: op, Err: os.ErrClosed} }

// cause returns the error a verbs call failed with, without the
// *verbs.OpError naming the libibverbs function, for the operation of the
// connection manager to report.
func cause(err error) error {
	var opErr *verbs.OpError
	if errors.As(err, &opErr) {
		return opErr.Err
	}
	return err
}

// resolveTimeout is the timeout of one resolution step: the time left until
// the deadline of ctx, or defaultResolveTimeout, in whole milliseconds.
func resolveTimeout(ctx context.Context) time.Duration {
	d := defaultResolveTimeout
	if deadline, ok := ctx.Deadline(); ok {
		d = time.Until(deadline)
	}
	return time.Duration(min(max(d.Milliseconds(), 1), 1<<31-1)) * time.Millisecond
}

// createID opens an event channel and creates an identifier of the TCP port
// space on it, as rdma_create_event_channel and rdma_create_id do.
func createID() (*ucma.Channel, uint32, uint64, error) {
	ch, err := ucma.Open()
	if err != nil {
		return nil, 0, 0, opError("rdma_create_event_channel", err)
	}
	uid := newUID()
	handle, err := ch.CreateID(uid, ucma.PortSpaceTCP, ucma.QPTypeRC)
	if err != nil {
		ch.Close()
		return nil, 0, 0, opError("rdma_create_id", err)
	}
	return ch, handle, uid, nil
}

// destroyID destroys an identifier and closes its channel.
func destroyID(ch *ucma.Channel, handle uint32) error {
	_, err := ch.DestroyID(handle)
	ch.Close()
	if err != nil {
		return opError("rdma_destroy_id", err)
	}
	return nil
}

// waitError is the error of a wait of op that GetEvent ended with err: that
// of ctx, os.ErrClosed once the channel is closed, or the GET_EVENT
// command's.
func waitError(ctx context.Context, op string, err error) error {
	if errors.Is(err, os.ErrClosed) {
		return errClosed(op)
	}
	if cerr := ctx.Err(); cerr != nil && err == cerr {
		return err
	}
	return opError("rdma_get_cm_event", err)
}

// ID is a communication identifier with an event channel of its own, an
// open descriptor of the kernel's rdma_cm device.
type ID struct {
	ch     *ucma.Channel
	handle uint32 // the kernel's number of the identifier
	uid    uint64
	turn   chan struct{} // held by the one wait taking events

	// Known once the address is resolved, and from the start for a request.
	dev    *device
	ctx    *verbs.Context // dev's context, whose Close releases nothing
	port   uint8
	local  netip.AddrPort
	remote netip.AddrPort

	disconnected atomic.Bool // a DISCONNECTED event was taken

	mu     sync.Mutex // serialises the commands and the queue pair's transitions with Close
	closed bool
	qp     *verbs.QP // as CreateQP returned it, whose Close destroys nothing
	ownQP  *verbs.QP // the same queue pair, destroyed by Close
	// The depths of the connection as rdma_connect and rdma_accept keep
	// them; those of the request on an incoming connection.
	initiatorDepth, responderResources uint8
	// connectError is set when the connect response failed here; the peer's
	// REJECTED and DISCONNECTED are not reported then.
	connectError bool
}

func newID(ch *ucma.Channel, handle uint32, uid uint64) *ID {
	return &ID{ch: ch, handle: handle, uid: uid, turn: make(chan struct{}, 1)}
}

// setDevice records the device and port of the identifier.
func (id *ID) setDevice(dev *device, port uint8) {
	id.dev, id.port, id.ctx = dev, port, verbs.ContextFromPointer(dev.ctx.Pointer())
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
	if err := initialize("rdma_resolve_addr"); err != nil {
		return nil, err
	}
	ch, handle, uid, err := createID()
	if err != nil {
		return nil, err
	}
	id := newID(ch, handle, uid)
	if err := ch.ResolveAddr(handle, local, remote, resolveTimeout(ctx)); err != nil {
		id.Close()
		return nil, opError("rdma_resolve_addr", err)
	}
	if _, err := id.awaitEvent(ctx, "rdma_resolve_addr", EventAddrResolved); err != nil {
		id.Close()
		return nil, err
	}
	if err := ch.ResolveRoute(handle, resolveTimeout(ctx)); err != nil {
		id.Close()
		return nil, opError("rdma_resolve_route", err)
	}
	if _, err := id.awaitEvent(ctx, "rdma_resolve_route", EventRouteResolved); err != nil {
		id.Close()
		return nil, err
	}
	return id, nil
}

// Device returns the device the connection uses. The package keeps it open
// for the life of the process; Close on it releases nothing.
func (id *ID) Device() *verbs.Context { return id.ctx }

// Port returns the device port the connection uses.
func (id *ID) Port() uint8 { return id.port }

// LocalAddr returns the local address of the connection.
func (id *ID) LocalAddr() netip.AddrPort { return id.local }

// RemoteAddr returns the address of the peer.
func (id *ID) RemoteAddr() netip.AddrPort { return id.remote }

// CreateQP creates the queue pair of the connection (rdma_create_qp) and
// stores the granted capacities in attr.Cap. pd must belong to Device(), nil
// meaning the device's default protection domain; attr.SendCQ and
// attr.RecvCQ are required and must outlive the ID, which destroys the queue
// pair when it closes.
//
// The queue pair belongs to the ID: Close on the *verbs.QP that CreateQP and
// QP return releases nothing. The QP of a verbs.AsyncEvent about the queue
// pair is the owning verbs object instead, whose Close destroys the queue
// pair under the ID.
func (id *ID) CreateQP(pd *verbs.PD, attr *verbs.QPInitAttr) (*verbs.QP, error) {
	const op = "rdma_create_qp"
	if attr.SendCQ == nil || attr.RecvCQ == nil {
		return nil, invalidArg(op, "send and receive completion queues are required")
	}
	id.mu.Lock()
	defer id.mu.Unlock()
	if id.closed {
		return nil, errClosed(op)
	}
	if id.ownQP != nil {
		return nil, opError(op, syscall.EINVAL)
	}
	if pd == nil {
		pd = id.dev.pd
	} else if pdContext(pd) != uintptr(id.dev.ctx.Pointer()) {
		return nil, opError(op, syscall.EINVAL)
	}
	qp, err := pd.CreateQP(attr)
	if err != nil {
		return nil, opError(op, cause(err))
	}
	if err := id.modify(qp, verbs.QPStateInit, nil); err != nil {
		qp.Close()
		return nil, opError(op, err)
	}
	id.ownQP, id.qp = qp, verbs.QPFromPointer(qp.Pointer())
	return id.qp, nil
}

// pdContext returns the address of the device context of pd, the first
// field of struct ibv_pd.
func pdContext(pd *verbs.PD) uintptr {
	p := pd.Pointer()
	if p == nil {
		return 0
	}
	return *(*uintptr)(p)
}

// QP returns the queue pair created by CreateQP, or nil.
func (id *ID) QP() *verbs.QP {
	id.mu.Lock()
	defer id.mu.Unlock()
	return id.qp
}

// modify moves qp to state with the attributes the kernel has for the
// identifier, as rdma_init_qp_attr and ibv_modify_qp do; adjust, if not nil,
// changes them first.
func (id *ID) modify(qp *verbs.QP, state verbs.QPState, adjust func(*verbs.QPAttr, *verbs.QPAttrMask)) error {
	k, err := id.ch.InitQPAttr(id.handle, uint32(state))
	if err != nil {
		return err
	}
	attr, mask := qpAttrFromKernel(state, &k)
	if adjust != nil {
		adjust(attr, &mask)
	}
	return cause(qp.Modify(attr, mask))
}

// modifyRTR moves the queue pair to RTR as ucma_modify_qp_rtr does: to INIT
// again, which brings the kernel's attributes up to date, then to RTR with
// the mask of the port's link layer and the responder resources.
func (id *ID) modifyRTR(responderResources uint8) error {
	if id.ownQP == nil {
		return nil
	}
	if err := id.modify(id.ownQP, verbs.QPStateInit, nil); err != nil {
		return err
	}
	return id.modify(id.ownQP, verbs.QPStateRTR, func(a *verbs.QPAttr, mask *verbs.QPAttrMask) {
		*mask = rtrMask(*mask, id.dev.linkLayer(id.port))
		overrideRTR(a, responderResources)
	})
}

// modifyRTS moves the queue pair to RTS with the initiator depth, as
// ucma_modify_qp_rts does.
func (id *ID) modifyRTS(initiatorDepth uint8) error {
	if id.ownQP == nil {
		return nil
	}
	return id.modify(id.ownQP, verbs.QPStateRTS, func(a *verbs.QPAttr, _ *verbs.QPAttrMask) {
		overrideRTS(a, initiatorDepth)
	})
}

// moveTo moves the queue pair, if there is one, to state.
func (id *ID) moveTo(state verbs.QPState) error {
	if id.ownQP == nil {
		return nil
	}
	return cause(id.ownQP.Modify(&verbs.QPAttr{State: state}, verbs.QPAttrState))
}

// Connect connects to the peer with the queue pair of CreateQP and waits until
// the connection is established; param may be nil for the defaults. It returns
// the peer's private data and the negotiated parameters, a *RejectedError when
// the peer rejects explicitly, a *verbs.OpError with syscall.ECONNREFUSED when
// nobody listens, syscall.ETIMEDOUT or syscall.EHOSTUNREACH when the peer is
// out of reach, and ctx.Err() when ctx ends first, after which the only
// sensible call on the ID is Close.
func (id *ID) Connect(ctx context.Context, param *ConnParam) (*ConnInfo, error) {
	ev, err := id.establish(ctx, "rdma_connect", param, MaxConnectPrivateData, id.connect)
	if err != nil {
		return nil, err
	}
	return &ConnInfo{PrivateData: ev.conn.PrivateData, ResponderResources: ev.conn.ResponderResources, InitiatorDepth: ev.conn.InitiatorDepth}, nil
}

// connect sends the connection request as rdma_connect does: the depths of p
// are checked and kept for the connection. The caller holds mu.
func (id *ID) connect(p ConnParam) error {
	const op = "rdma_connect"
	if err := checkDepths(p, id.dev.limits); err != nil {
		return opError(op, err)
	}
	id.initiatorDepth, id.responderResources = connectDepths(p, id.dev.limits)
	cp := kernelConnParam(p, id.ownQP.Num(), id.initiatorDepth, id.responderResources)
	if err := id.ch.Connect(id.handle, cp, ucma.ECE{}); err != nil {
		return opError(op, err)
	}
	return nil
}

// establish runs call, the connect or accept named op, with the parameters
// of param, whose private data may have max bytes, and waits for the
// ESTABLISHED event.
func (id *ID) establish(ctx context.Context, op string, param *ConnParam, max int, call func(ConnParam) error) (*event, error) {
	if err := checkConnParam(op, param, max); err != nil {
		return nil, err
	}
	id.mu.Lock()
	if id.closed {
		id.mu.Unlock()
		return nil, errClosed(op)
	}
	if id.ownQP == nil {
		id.mu.Unlock()
		return nil, invalidArg(op, "no queue pair, call CreateQP first")
	}
	err := call(param.withDefaults())
	id.mu.Unlock()
	if err != nil {
		return nil, err
	}
	return id.awaitEvent(ctx, op, EventEstablished)
}

// never is the condition of a wait that only events end.
func never() bool { return false }

// awaitEvent takes events until want arrives and returns it; an event that
// ends the attempt returns its error instead, other events are skipped.
func (id *ID) awaitEvent(ctx context.Context, op string, want Event) (*event, error) {
	var got *event
	err := id.wait(ctx, op, never, func(ev *event) (bool, error) {
		if ev.typ == want {
			got = ev
			return true, nil
		}
		return false, ev.err(op)
	})
	if err != nil {
		return nil, err
	}
	return got, nil
}

// wait takes the events of the ID, one waiter at a time, and hands each to
// take until take is done or fails, done reports true before an event, ctx
// ends or the ID closes. The waits hold no OS thread.
func (id *ID) wait(ctx context.Context, op string, done func() bool, take func(*event) (bool, error)) error {
	if err := ctx.Err(); err != nil {
		return err
	}
	select {
	case id.turn <- struct{}{}:
	case <-ctx.Done():
		return ctx.Err()
	}
	defer func() { <-id.turn }()
	for !done() {
		ev, err := id.next(ctx, op)
		if err != nil {
			return err
		}
		if ok, err := take(ev); ok || err != nil {
			return err
		}
	}
	return nil
}

// next fetches the next event of the ID and returns it as
// rdma_get_cm_event hands it out, skipping those it discards.
func (id *ID) next(ctx context.Context, op string) (*event, error) {
	for {
		uev, err := id.ch.GetEvent(ctx)
		if err != nil {
			return nil, waitError(ctx, op, err)
		}
		if !known(uev, id.uid, id.handle) {
			continue
		}
		if ev, err := id.process(uev, op); err != nil || ev != nil {
			return ev, err
		}
	}
}

// process does what rdma_get_cm_event does on an event of the ID and returns
// the event it hands out, or nil for one it discards: the addresses and the
// device once the address is resolved, the queue pair's way to RTS and the
// ACCEPT that ends a connect response, the queue pair's move to ERR on a
// reject.
func (id *ID) process(uev ucma.Event, op string) (*event, error) {
	ev := &event{typ: Event(uev.Type), status: int(uev.Status), conn: uev.Conn()}
	id.mu.Lock()
	defer id.mu.Unlock()
	if id.closed {
		return nil, errClosed(op)
	}
	switch ev.typ {
	case EventAddrResolved:
		if id.queryAddr() != nil {
			ev.typ, ev.status = EventAddrError, -1
		}
	case EventConnectResponse:
		if id.ownQP != nil {
			if id.connectResponse() != nil {
				ev.typ, ev.status = EventConnectError, -1
				id.connectError = true
			} else {
				ev.typ = EventEstablished
			}
		}
	case EventRejected:
		if id.connectError {
			return nil, nil
		}
		id.moveTo(verbs.QPStateErr)
	case EventDisconnected:
		if id.connectError {
			return nil, nil
		}
		id.disconnected.Store(true)
	}
	return ev, nil
}

// queryAddr records the addresses, the device and the port of the
// identifier, as ucma_query_addr does once the address is resolved; an
// identifier without a device is ENODEV.
func (id *ID) queryAddr() error {
	info, err := id.ch.QueryAddr(id.handle)
	if err != nil {
		return err
	}
	id.local, id.remote = info.Src, info.Dst
	if id.dev == nil && info.NodeGUID != 0 {
		dev, err := lookupDevice(info.NodeGUID, info.DevIndex)
		if err != nil {
			return err
		}
		id.setDevice(dev, info.PortNum)
	}
	if id.dev == nil {
		return syscall.ENODEV
	}
	return nil
}

// connectResponse completes the connection the peer accepted, as
// ucma_process_conn_resp does: RTR and RTS with the kernel's attributes, then
// the ACCEPT without parameters that tells the peer the queue pair is ready.
// A failure moves the queue pair to ERR.
func (id *ID) connectResponse() error {
	err := id.modifyRTR(maxDepth)
	if err == nil {
		err = id.modifyRTS(maxDepth)
	}
	if err == nil {
		err = id.ch.Accept(id.handle, 0, nil, ucma.ECE{})
	}
	if err != nil {
		id.moveTo(verbs.QPStateErr)
	}
	return err
}

// Disconnect disconnects from the peer (rdma_disconnect) and waits for the
// DISCONNECTED event, unless Wait or an earlier Disconnect already consumed
// the peer's. The queue pair moves to the error state.
func (id *ID) Disconnect(ctx context.Context) error {
	const op = "rdma_disconnect"
	id.mu.Lock()
	if id.closed {
		id.mu.Unlock()
		return errClosed(op)
	}
	err := id.disconnect()
	id.mu.Unlock()
	if err != nil {
		return opError(op, err)
	}
	return id.wait(ctx, op, id.disconnected.Load, func(ev *event) (bool, error) {
		if ev.typ == EventDeviceRemoval {
			return false, ev.err(op)
		}
		return false, nil
	})
}

// disconnect moves the queue pair to the state the transport takes and
// disconnects, as rdma_disconnect does. The caller holds mu.
func (id *ID) disconnect() error {
	state, err := shutdownState(id.dev.transport)
	if err != nil {
		return err
	}
	if err := id.moveTo(state); err != nil {
		return err
	}
	return id.ch.Disconnect(id.handle)
}

// Wait waits for the next event of the connection, such as EventDisconnected
// when the peer disconnects, EventTimewaitExit after a disconnect,
// EventAddrChange or EventDeviceRemoval. It returns ctx.Err() when ctx ends
// first and an error wrapping os.ErrClosed once the ID is closed.
func (id *ID) Wait(ctx context.Context) (Event, error) {
	const op = "rdma_get_cm_event"
	id.mu.Lock()
	closed := id.closed
	id.mu.Unlock()
	if closed {
		return 0, errClosed(op)
	}
	var got Event
	err := id.wait(ctx, op, never, func(ev *event) (bool, error) {
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
	if id.ownQP != nil {
		id.ownQP.Close()
		id.ownQP, id.qp = nil, nil
	}
	return destroyID(id.ch, id.handle)
}

// Listener listens for connection requests on an address.
type Listener struct {
	ch     *ucma.Channel
	handle uint32
	uid    uint64
	addr   netip.AddrPort

	mu     sync.Mutex // serialises taking a request with Close
	closed bool
}

// Listen binds local, whose IP picks the device and may be unspecified, and
// listens for connection requests; port 0 picks a free port. backlog bounds
// the requests waiting to be accepted, 0 meaning the kernel's maximum.
func Listen(local netip.AddrPort, backlog int) (*Listener, error) {
	if err := checkListenAddr(local); err != nil {
		return nil, err
	}
	if err := initialize("rdma_listen"); err != nil {
		return nil, err
	}
	ch, handle, uid, err := createID()
	if err != nil {
		return nil, err
	}
	if err := bind(ch, handle, local); err != nil {
		destroyID(ch, handle)
		return nil, opError("rdma_bind_addr", err)
	}
	if err := ch.Listen(handle, backlog); err != nil {
		destroyID(ch, handle)
		return nil, opError("rdma_listen", err)
	}
	info, err := ch.QueryAddr(handle)
	if err != nil {
		destroyID(ch, handle)
		return nil, opError("rdma_listen", err)
	}
	return &Listener{ch: ch, handle: handle, uid: uid, addr: info.Src}, nil
}

// bind binds an identifier to addr as rdma_bind_addr does, opening the
// device that addr picks, if it picks one.
func bind(ch *ucma.Channel, handle uint32, addr netip.AddrPort) error {
	if err := ch.Bind(handle, addr); err != nil {
		return err
	}
	info, err := ch.QueryAddr(handle)
	if err != nil {
		return err
	}
	if info.NodeGUID != 0 {
		_, err = lookupDevice(info.NodeGUID, info.DevIndex)
	}
	return err
}

// Addr returns the bound address, with the port actually chosen for port 0.
func (l *Listener) Addr() netip.AddrPort { return l.addr }

// Accept waits for the next connection request and returns it with its
// private data and parameters; the request's ID is on an event channel of its
// own. It returns ctx.Err() when ctx ends first and an error wrapping
// net.ErrClosed once the Listener is closed.
func (l *Listener) Accept(ctx context.Context) (*Request, error) {
	req, err := l.accept(ctx)
	if errors.Is(err, os.ErrClosed) {
		return nil, &verbs.OpError{Op: "rdma_get_cm_event", Err: net.ErrClosed}
	}
	return req, err
}

func (l *Listener) accept(ctx context.Context) (*Request, error) {
	const op = "rdma_get_cm_event"
	l.mu.Lock()
	closed := l.closed
	l.mu.Unlock()
	if closed {
		return nil, os.ErrClosed
	}
	for {
		uev, err := l.ch.GetEvent(ctx)
		if err != nil {
			return nil, waitError(ctx, op, err)
		}
		if !known(uev, l.uid, l.handle) {
			continue
		}
		switch Event(uev.Type) {
		case EventConnectRequest:
			if req, err := l.request(uev); err != nil || req != nil {
				return req, err
			}
		case EventDeviceRemoval:
			ev := event{typ: EventDeviceRemoval, status: int(uev.Status)}
			return nil, ev.err(op)
		}
	}
}

// request turns a CONNECT_REQUEST event into a Request whose ID is on an
// event channel of its own, as ucma_process_conn_req and rdma_migrate_id do.
// A request whose identifier the query of its addresses and device fails on
// is destroyed and skipped, which a nil Request reports.
func (l *Listener) request(uev ucma.Event) (*Request, error) {
	l.mu.Lock()
	defer l.mu.Unlock()
	if l.closed {
		return nil, os.ErrClosed
	}
	info, dev, err := describe(l.ch, uev.ID)
	if err != nil {
		l.ch.DestroyID(uev.ID)
		return nil, nil
	}
	ch, err := ucma.Open()
	if err != nil {
		l.ch.DestroyID(uev.ID)
		return nil, opError("rdma_create_event_channel", err)
	}
	if _, err := ch.MigrateID(l.ch, uev.ID); err != nil {
		l.ch.DestroyID(uev.ID)
		ch.Close()
		return nil, opError("rdma_migrate_id", err)
	}
	conn := uev.Conn()
	id := newID(ch, uev.ID, newUID())
	id.setDevice(dev, info.PortNum)
	id.local, id.remote = info.Src, info.Dst
	id.initiatorDepth, id.responderResources = conn.InitiatorDepth, conn.ResponderResources
	return &Request{ID: id, PrivateData: conn.PrivateData, ResponderResources: conn.ResponderResources, InitiatorDepth: conn.InitiatorDepth}, nil
}

// describe queries the addresses and the device of the identifier of a
// request, as ucma_query_req_info does; one without a device is ENODEV.
func describe(ch *ucma.Channel, handle uint32) (ucma.AddrInfo, *device, error) {
	info, err := ch.QueryAddr(handle)
	if err != nil {
		return info, nil, err
	}
	if info.NodeGUID == 0 {
		return info, nil, syscall.ENODEV
	}
	dev, err := lookupDevice(info.NodeGUID, info.DevIndex)
	return info, dev, err
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
	return destroyID(l.ch, l.handle)
}

// Accept accepts the request with the queue pair created on its ID and waits
// until the connection is established; param may be nil for the defaults.
// The private data goes to the peer's Connect.
func (r *Request) Accept(ctx context.Context, param *ConnParam) error {
	id := r.ID
	_, err := id.establish(ctx, "rdma_accept", param, MaxAcceptPrivateData, id.accept)
	return err
}

// accept accepts the request as rdma_accept does: the depths of p are
// checked and kept, the queue pair moves to RTR and RTS, and the ACCEPT
// gives the identifier its uid; a failed ACCEPT moves the queue pair to ERR.
// The caller holds mu.
func (id *ID) accept(p ConnParam) error {
	const op = "rdma_accept"
	if err := checkDepths(p, id.dev.limits); err != nil {
		return opError(op, err)
	}
	id.initiatorDepth, id.responderResources = acceptDepths(p, id.initiatorDepth, id.responderResources, id.dev.limits)
	if err := id.modifyRTR(id.responderResources); err != nil {
		return opError(op, err)
	}
	if err := id.modifyRTS(id.initiatorDepth); err != nil {
		return opError(op, err)
	}
	cp := kernelConnParam(p, id.ownQP.Num(), id.initiatorDepth, id.responderResources)
	if err := id.ch.Accept(id.handle, id.uid, &cp, ucma.ECE{}); err != nil {
		id.moveTo(verbs.QPStateErr)
		return opError(op, err)
	}
	return nil
}

// Reject rejects the request, handing privateData to the peer's Connect as a
// *RejectedError, and closes the request's ID.
func (r *Request) Reject(privateData []byte) error {
	const op = "rdma_reject"
	if err := checkPrivateData(op, privateData, MaxRejectPrivateData); err != nil {
		return err
	}
	id := r.ID
	id.mu.Lock()
	if id.closed {
		id.mu.Unlock()
		return errClosed(op)
	}
	err := id.ch.Reject(id.handle, 0, privateData)
	id.mu.Unlock()
	if err != nil {
		return opError(op, err)
	}
	return id.Close()
}
