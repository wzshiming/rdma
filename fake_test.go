package rdma

import (
	"context"
	"errors"
	"fmt"
	"net"
	"net/netip"
	"sync"
	"sync/atomic"
	"testing"

	"github.com/wzshiming/rdma/cm"
	"github.com/wzshiming/rdma/verbs"
)

// fakeFabric is an in-memory fabric: listeners are found by address, and a
// fakeTransport hands each message straight to its peer's next posted
// receive buffer. A message that finds no posted buffer is a protocol bug
// (invariant I1) and is reported through fail, so the tests catch it instead
// of the RNR retry logic of a real device hiding it.
type fakeFabric struct {
	fail       func(format string, args ...any)
	registered atomic.Int64 // bytes of buffer memory alive, the fake's registered memory

	mu        sync.Mutex
	listeners map[netip.AddrPort]*fakeAcceptor
	nextPort  uint16
}

func newFakeFabric(t testing.TB) *fakeFabric {
	return &fakeFabric{
		fail: func(format string, args ...any) {
			t.Helper()
			t.Errorf("fake transport: "+format, args...)
		},
		listeners: make(map[netip.AddrPort]*fakeAcceptor),
		nextPort:  40000,
	}
}

// fakeIP is the address of every endpoint of the fake fabric.
var fakeIP = netip.MustParseAddr("10.99.0.1")

func (f *fakeFabric) port() uint16 {
	f.nextPort++
	return f.nextPort
}

func (f *fakeFabric) listen(local netip.AddrPort, backlog int) (acceptor, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	if local.Port() == 0 {
		local = netip.AddrPortFrom(local.Addr(), f.port())
	}
	if _, taken := f.listeners[local]; taken {
		return nil, &verbs.OpError{Op: "rdma_bind_addr", Err: errAddrInUse}
	}
	if backlog <= 0 {
		backlog = 16
	}
	a := &fakeAcceptor{f: f, local: local, reqs: make(chan *fakeRequest, backlog), done: make(chan struct{})}
	f.listeners[local] = a
	return a, nil
}

// lookup finds the listener for remote, exact or wildcard.
func (f *fakeFabric) lookup(remote netip.AddrPort) *fakeAcceptor {
	f.mu.Lock()
	defer f.mu.Unlock()
	if a := f.listeners[remote]; a != nil {
		return a
	}
	for addr, a := range f.listeners {
		if addr.Addr().IsUnspecified() && addr.Port() == remote.Port() {
			return a
		}
	}
	return nil
}

func (f *fakeFabric) dial(ctx context.Context, local, remote netip.AddrPort, cfg Config, hello []byte) (transport, []byte, error) {
	a := f.lookup(remote)
	if a == nil {
		return nil, nil, &verbs.OpError{Op: "rdma_connect", Err: errRefused}
	}
	if !local.Addr().IsValid() {
		f.mu.Lock()
		local = netip.AddrPortFrom(fakeIP, f.port())
		f.mu.Unlock()
	}
	ep := newFakeTransport(f, cfg, local, remote)
	req := &fakeRequest{a: a, ep: ep, data: pad(hello, cm.MaxConnectPrivateData), reply: make(chan fakeReply, 1)}
	select {
	case a.reqs <- req:
	case <-a.done:
		ep.close()
		return nil, nil, &verbs.OpError{Op: "rdma_connect", Err: errRefused}
	case <-ctx.Done():
		ep.close()
		return nil, nil, ctx.Err()
	}
	select {
	case r := <-req.reply:
		if r.err != nil {
			ep.close()
			return nil, nil, r.err
		}
		return ep, r.data, nil
	case <-a.done:
		ep.close()
		return nil, nil, &verbs.OpError{Op: "rdma_connect", Err: errRefused}
	case <-ctx.Done():
		ep.close()
		return nil, nil, ctx.Err()
	}
}

// pad zero pads data to the size of the private data field, as the IB CM
// delivers it.
func pad(data []byte, size int) []byte {
	b := make([]byte, size)
	copy(b, data)
	return b
}

type fakeAcceptor struct {
	f     *fakeFabric
	local netip.AddrPort
	reqs  chan *fakeRequest
	done  chan struct{}
	once  sync.Once
}

func (a *fakeAcceptor) addr() netip.AddrPort { return a.local }

func (a *fakeAcceptor) accept(ctx context.Context) (request, error) {
	select {
	case <-a.done:
		return nil, &verbs.OpError{Op: "rdma_get_cm_event", Err: net.ErrClosed}
	default:
	}
	select {
	case req := <-a.reqs:
		return req, nil
	case <-a.done:
		return nil, &verbs.OpError{Op: "rdma_get_cm_event", Err: net.ErrClosed}
	case <-ctx.Done():
		return nil, ctx.Err()
	}
}

func (a *fakeAcceptor) close() error {
	a.once.Do(func() {
		a.f.mu.Lock()
		delete(a.f.listeners, a.local)
		a.f.mu.Unlock()
		close(a.done)
	})
	return nil
}

type fakeReply struct {
	data []byte
	err  error
}

type fakeRequest struct {
	a     *fakeAcceptor
	ep    *fakeTransport // the dialer's end
	data  []byte
	reply chan fakeReply
}

func (r *fakeRequest) hello() []byte              { return r.data }
func (r *fakeRequest) remoteAddr() netip.AddrPort { return r.ep.local }

func (r *fakeRequest) accept(ctx context.Context, cfg Config, reply []byte) (transport, error) {
	local := r.a.local
	if local.Addr().IsUnspecified() {
		local = netip.AddrPortFrom(fakeIP, local.Port())
	}
	ep := newFakeTransport(r.a.f, cfg, local, r.ep.local)
	ep.link(r.ep)
	r.ep.link(ep)
	r.reply <- fakeReply{data: pad(reply, cm.MaxAcceptPrivateData)}
	return ep, nil
}

func (r *fakeRequest) reject(data []byte) error {
	r.reply <- fakeReply{err: &cm.RejectedError{PrivateData: pad(data, cm.MaxRejectPrivateData), Reason: 28}}
	return nil
}

// fakeTransport is one end of an in-memory connection.
type fakeTransport struct {
	f             *fakeFabric
	local, remote netip.AddrPort
	send, recv    [][]byte

	mu     sync.Mutex
	peer   *fakeTransport
	posted []int // receive slots offered, oldest first
	done   []completion
	wake   chan struct{} // holds one pending wake-up
	err    error         // the peer is gone
	closed bool
}

func newFakeTransport(f *fakeFabric, cfg Config, local, remote netip.AddrPort) *fakeTransport {
	t := &fakeTransport{f: f, local: local, remote: remote, wake: make(chan struct{}, 1)}
	t.send = slots(cfg.SendDepth+1, cfg.MessageSize)
	t.recv = slots(cfg.ReceiveDepth+1, cfg.MessageSize)
	for i := range t.recv {
		t.posted = append(t.posted, i)
	}
	f.registered.Add(int64((cfg.SendDepth + cfg.ReceiveDepth + 2) * cfg.MessageSize))
	return t
}

func slots(n, size int) [][]byte {
	mem := make([]byte, n*size)
	s := make([][]byte, n)
	for i := range s {
		s[i] = mem[i*size : (i+1)*size : (i+1)*size]
	}
	return s
}

func (t *fakeTransport) link(peer *fakeTransport) {
	t.mu.Lock()
	t.peer = peer
	t.mu.Unlock()
}

func (t *fakeTransport) sendSlot(i int) []byte { return t.send[i] }
func (t *fakeTransport) recvSlot(i int) []byte { return t.recv[i] }

func (t *fakeTransport) postSend(i, n int) error {
	t.mu.Lock()
	peer, closed := t.peer, t.closed
	t.mu.Unlock()
	if closed {
		return errors.New("fake transport: postSend on a closed transport")
	}
	if peer == nil {
		return errors.New("fake transport: postSend before the connection is up")
	}
	peer.deliver(t.send[i][:n])
	t.mu.Lock()
	t.done = append(t.done, completion{send: true, slot: i})
	t.mu.Unlock()
	t.signal()
	return nil
}

// deliver lands a message in the next posted receive buffer.
func (t *fakeTransport) deliver(msg []byte) {
	t.mu.Lock()
	defer t.mu.Unlock()
	if t.closed {
		return
	}
	if len(t.posted) == 0 {
		t.f.fail("a %d-byte message arrived at %v with no posted receive buffer", len(msg), t.local)
		return
	}
	slot := t.posted[0]
	t.posted = t.posted[1:]
	if len(msg) > len(t.recv[slot]) {
		t.f.fail("a %d-byte message overflows the %d-byte receive buffer at %v", len(msg), len(t.recv[slot]), t.local)
		return
	}
	copy(t.recv[slot], msg)
	t.done = append(t.done, completion{slot: slot, n: len(msg)})
	t.signal()
}

func (t *fakeTransport) postRecv(i int) error {
	t.mu.Lock()
	defer t.mu.Unlock()
	if t.closed {
		return errors.New("fake transport: postRecv on a closed transport")
	}
	for _, p := range t.posted {
		if p == i {
			t.f.fail("receive buffer %d posted twice at %v", i, t.local)
		}
	}
	t.posted = append(t.posted, i)
	return nil
}

func (t *fakeTransport) signal() {
	select {
	case t.wake <- struct{}{}:
	default:
	}
}

func (t *fakeTransport) wait(ctx context.Context, wcs []completion) (int, error) {
	for {
		t.mu.Lock()
		if len(t.done) > 0 {
			n := copy(wcs, t.done)
			t.done = append(t.done[:0], t.done[n:]...)
			t.mu.Unlock()
			return n, nil
		}
		if t.err != nil {
			err := t.err
			t.mu.Unlock()
			return 0, err
		}
		if t.closed {
			t.mu.Unlock()
			return 0, errors.New("fake transport: wait on a closed transport")
		}
		t.mu.Unlock()
		select {
		case <-t.wake:
		case <-ctx.Done():
			return 0, ctx.Err()
		}
	}
}

// fail is what the peer's disconnect or close does to this end.
func (t *fakeTransport) fail(err error) {
	t.mu.Lock()
	if t.err == nil {
		t.err = err
	}
	t.mu.Unlock()
	t.signal()
}

func (t *fakeTransport) disconnect(ctx context.Context) error {
	t.mu.Lock()
	peer := t.peer
	t.mu.Unlock()
	if peer != nil {
		peer.fail(errConnReset)
	}
	return nil
}

func (t *fakeTransport) close() error {
	t.mu.Lock()
	if t.closed {
		t.mu.Unlock()
		return nil
	}
	t.closed = true
	peer := t.peer
	t.mu.Unlock()
	if peer != nil {
		peer.fail(errConnReset)
	}
	t.f.registered.Add(-int64((len(t.send) + len(t.recv)) * len(t.send[0])))
	t.signal()
	return nil
}

func (t *fakeTransport) localAddr() netip.AddrPort  { return t.local }
func (t *fakeTransport) remoteAddr() netip.AddrPort { return t.remote }

// fakePipe connects two engines over the fake fabric, like a listener and a
// dialer would, with cfg on both sides.
func fakePipe(t testing.TB, f *fakeFabric, cfg Config) (client, server *Conn) {
	t.Helper()
	cfg = cfg.withDefaults()
	a, err := f.listen(netip.AddrPortFrom(fakeIP, 0), 1)
	if err != nil {
		t.Fatalf("fake listen: %v", err)
	}
	defer a.close()
	type dialed struct {
		tr    transport
		reply []byte
		err   error
	}
	ch := make(chan dialed, 1)
	go func() {
		tr, reply, err := f.dial(context.Background(), netip.AddrPort{}, a.addr(), cfg, encodeHello(cfg))
		ch <- dialed{tr, reply, err}
	}()
	req, err := a.accept(context.Background())
	if err != nil {
		t.Fatalf("fake accept: %v", err)
	}
	h, err := decodeHello(req.hello())
	if err != nil {
		t.Fatalf("decodeHello: %v", err)
	}
	negotiated, err := negotiate(cfg, h)
	if err != nil {
		t.Fatalf("negotiate: %v", err)
	}
	str, err := req.accept(context.Background(), negotiated, encodeHello(negotiated))
	if err != nil {
		t.Fatalf("fake request accept: %v", err)
	}
	d := <-ch
	if d.err != nil {
		t.Fatalf("fake dial: %v", d.err)
	}
	return newConn(d.tr, negotiated), newConn(str, negotiated)
}

// String makes test failures readable.
func (t *fakeTransport) String() string { return fmt.Sprintf("fake %v->%v", t.local, t.remote) }
