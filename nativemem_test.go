package rdma

import (
	"context"
	"errors"
	"net"
	"net/netip"
	"sync"
	"testing"

	"github.com/wzshiming/rdma/internal/rsproto"
)

// nativeOnMemLink is the native engine's stream on the memory link of
// rsproto.
var nativeOnMemLink = engineUnderTest{name: "native", new: newMemEngine, addr: "10.99.0.1:0", owns: memOwns}

// memEngine connects native streams over memory links of rsproto, each end
// on a memTransport. It fails the test when the memory of a stream is never
// released.
type memEngine struct {
	fail func(format string, args ...any)

	mu        sync.Mutex
	listeners map[uint16]*memAcceptor
	nextPort  uint16
	ends      []*memTransport
}

func newMemEngine(t testing.TB) engine {
	e := &memEngine{
		fail: func(format string, args ...any) {
			t.Helper()
			t.Errorf("memory engine: "+format, args...)
		},
		listeners: make(map[uint16]*memAcceptor),
		nextPort:  40000,
	}
	t.Cleanup(e.checkReleased)
	return e
}

func (e *memEngine) checkReleased() {
	e.mu.Lock()
	ends := e.ends
	e.mu.Unlock()
	for _, t := range ends {
		if !t.isReleased() {
			e.fail("the memory of a stream was never released")
		}
	}
}

func (e *memEngine) dial(ctx context.Context, laddr, raddr *Addr, c Config) (stream, error) {
	e.mu.Lock()
	defer e.mu.Unlock()
	a := e.listeners[uint16(raddr.Port)]
	if a == nil {
		return nil, errRefused
	}
	e.nextPort++
	client, server, err := e.connect(c, a.cfg, netip.AddrPortFrom(fakeIP, e.nextPort), a.bound)
	if err != nil {
		return nil, err
	}
	select {
	case a.pending <- server:
	default:
		client.close()
		server.close()
		return nil, errRefused
	}
	return client, nil
}

// connect joins a native stream on the memory of each Config over a new
// memory link, as the handshake of rsockets does: each side takes the
// connection data of the other. The caller holds e.mu.
func (e *memEngine) connect(cc, sc Config, caddr, saddr netip.AddrPort) (client, server *nativeStream, err error) {
	la, lb := rsproto.NewMemLink()
	ta, tb := newMemTransport(la, memConfig(cc)), newMemTransport(lb, memConfig(sc))
	ta.peer, tb.peer = tb, ta
	e.ends = append(e.ends, ta, tb)
	if client, err = newNativeStream(ta, ta.mem, caddr, saddr); err != nil {
		return nil, nil, err
	}
	if server, err = newNativeStream(tb, tb.mem, saddr, caddr); err != nil {
		return nil, nil, err
	}
	client.connect(e.handshake(server))
	server.connect(e.handshake(client))
	return client, server, nil
}

// handshake returns the connection data of s as its peer reads it from the
// private data.
func (e *memEngine) handshake(s *nativeStream) rsproto.ConnData {
	b := s.connData().Encode()
	peer, err := rsproto.DecodeConnData(b[:])
	if err != nil {
		e.fail("DecodeConnData: %v", err)
	}
	return peer
}

func (e *memEngine) listen(laddr *Addr, backlog int, c Config) (acceptor, error) {
	e.mu.Lock()
	defer e.mu.Unlock()
	port := uint16(laddr.Port)
	if port == 0 {
		e.nextPort++
		port = e.nextPort
	}
	if e.listeners[port] != nil {
		return nil, errAddrInUse
	}
	a := &memAcceptor{
		e:       e,
		bound:   netip.AddrPortFrom(laddr.addrPort().Addr(), port),
		cfg:     c,
		pending: make(chan *nativeStream, 16),
		closing: make(chan struct{}),
	}
	e.listeners[port] = a
	return a, nil
}

// memConfig lays out the memory and queues of one end for c: the sizes c
// asks for, the defaults of rsockets for zero, a send buffer at least as
// large as rs_can_send wants free and a receive buffer of an even length,
// as it is granted in halves.
func memConfig(c Config) rsproto.Config {
	region := func(i uint32, n int) rsproto.Region {
		return rsproto.Region{Buf: make([]byte, n), Addr: uint64(i) << 40, Key: i}
	}
	rbuf := sizeOr(c.ReceiveBufferSize, rsproto.DefaultReceiveBufferSize)
	return rsproto.Config{
		SendBuffer:       region(1, max(sizeOr(c.SendBufferSize, rsproto.DefaultSendBufferSize), rsproto.SndLowat)),
		ReceiveBuffer:    region(2, rbuf+rbuf%2),
		GrantTable:       region(3, rsproto.SGLSize*rsproto.SGESize),
		ControlBuffer:    region(4, rsproto.QPCtrlSize*rsproto.SGESize),
		SendQueueSize:    queueSize(c.SendQueueSize, rsproto.DefaultSendQueueSize),
		ReceiveQueueSize: queueSize(c.ReceiveQueueSize, rsproto.DefaultReceiveQueueSize),
	}
}

// queueSize returns the depth n asks for, def for zero, within the bounds
// rs_set_qp_size keeps.
func queueSize(n, def int) int {
	return min(max(sizeOr(n, def), rsproto.QPMinSize), rsproto.QPMaxSize)
}

type memAcceptor struct {
	e       *memEngine
	bound   netip.AddrPort
	cfg     Config
	pending chan *nativeStream
	closing chan struct{}
	once    sync.Once
}

func (a *memAcceptor) addr() netip.AddrPort { return a.bound }

func (a *memAcceptor) accept() (stream, error) {
	select {
	case <-a.closing:
		return nil, net.ErrClosed
	default:
	}
	select {
	case s := <-a.pending:
		return s, nil
	case <-a.closing:
		return nil, net.ErrClosed
	}
}

func (a *memAcceptor) close() error {
	a.once.Do(func() {
		close(a.closing)
		a.e.mu.Lock()
		delete(a.e.listeners, a.bound.Port())
		a.e.mu.Unlock()
		// Connections nobody accepted go down with the listener.
		for {
			select {
			case s := <-a.pending:
				s.close()
			default:
				return
			}
		}
	})
	return nil
}

// errTakenDown is the loss of the link a memTransport's down causes.
var errTakenDown = errors.New("memory link taken down")

// memTransport is the transport of a native stream on one end of a memory
// link of rsproto. A goroutine of its own feeds the stream what happens at
// its end, as a completion queue would; posts at either end poke it.
type memTransport struct {
	end  *rsproto.MemEnd
	peer *memTransport
	mem  rsproto.Config
	kick chan struct{} // holds a poke not yet taken
	stop chan struct{} // closed by down
	done chan struct{} // closed when the pump returns

	ev   linkEvents // set by start, for the pump
	lost bool       // the pump has fed the loss of the link

	silence sync.Mutex // held, it keeps the pump from taking anything, as a peer gone silent

	mu       sync.Mutex
	started  bool
	released bool
}

func newMemTransport(end *rsproto.MemEnd, mem rsproto.Config) *memTransport {
	for _, r := range []rsproto.Region{mem.SendBuffer, mem.ReceiveBuffer, mem.GrantTable, mem.ControlBuffer} {
		end.Register(r)
	}
	return &memTransport{end: end, mem: mem, kick: make(chan struct{}, 1), stop: make(chan struct{}), done: make(chan struct{})}
}

// poke makes the pump look at the end again.
func (t *memTransport) poke() {
	select {
	case t.kick <- struct{}{}:
	default:
	}
}

// PostWriteMsg puts the write on the link. The peer takes it; a link down
// completes it here at once.
func (t *memTransport) PostWriteMsg(id uint64, sgl []rsproto.Segment, msg rsproto.Msg, addr uint64, key uint32) error {
	err := t.end.PostWriteMsg(id, sgl, msg, addr, key)
	t.peer.poke()
	t.poke()
	return err
}

// PostMsg puts the message word on the link, as PostWriteMsg does.
func (t *memTransport) PostMsg(id uint64, msg rsproto.Msg) error {
	err := t.end.PostMsg(id, msg)
	t.peer.poke()
	t.poke()
	return err
}

// PostRecv posts a receive, which a message the peer sent may have waited
// for.
func (t *memTransport) PostRecv(id uint64) error {
	err := t.end.PostRecv(id)
	t.poke()
	return err
}

func (t *memTransport) start(ev linkEvents) {
	t.mu.Lock()
	t.started = true
	t.mu.Unlock()
	t.ev = ev
	go t.pump()
}

func (t *memTransport) pump() {
	defer close(t.done)
	for {
		select {
		case <-t.kick:
		case <-t.stop:
			return
		}
		t.silence.Lock()
		for t.step() {
		}
		t.silence.Unlock()
	}
}

// step feeds the stream, under its lock, the messages that reach this end,
// the completions of its sends and, once they are all taken, the loss of the
// link, and reports whether there was any. Deliver writes the memory of the
// stream, so the lock covers it.
func (t *memTransport) step() bool {
	t.ev.lock()
	defer t.ev.unlock()
	moved := false
	for msg, ok := t.end.Deliver(); ok; msg, ok = t.end.Deliver() {
		t.ev.received(msg)
		t.peer.poke()
		moved = true
	}
	for id, err, ok := t.end.Completion(); ok; id, err, ok = t.end.Completion() {
		t.ev.sent(id, err)
		moved = true
	}
	if err := t.end.Down(); err != nil && !t.lost {
		t.lost = true
		t.ev.lost(err)
		moved = true
	}
	return moved
}

// down breaks the link, which both ends learn as its loss, and stops the
// pump.
func (t *memTransport) down() {
	t.end.Break(errTakenDown)
	t.peer.poke()
	close(t.stop)
	t.mu.Lock()
	started := t.started
	t.mu.Unlock()
	if started {
		<-t.done
	}
}

func (t *memTransport) release() {
	t.mu.Lock()
	defer t.mu.Unlock()
	t.released = true
}

// breakLink loses the link without an end of the connection, as a cut cable
// does.
func (t *memTransport) breakLink(err error) {
	t.end.Break(err)
	t.poke()
	t.peer.poke()
}

func (t *memTransport) isReleased() bool {
	t.mu.Lock()
	defer t.mu.Unlock()
	return t.released
}

// memTransportOf returns the transport under c, a connection of a
// memEngine.
func memTransportOf(c *Conn) *memTransport { return c.s.(*nativeStream).t.(*memTransport) }

// memOwns reports whether p lies in the send or the receive buffer of s, a
// native stream on a memTransport.
func memOwns(s stream, p []byte) bool {
	ns, ok := s.(*nativeStream)
	if !ok {
		return false
	}
	mem := ns.t.(*memTransport).mem
	return within(p, mem.SendBuffer.Buf) || within(p, mem.ReceiveBuffer.Buf)
}
