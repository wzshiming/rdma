package rdma

import (
	"context"
	"errors"
	"net"
	"net/netip"
	"sync"
	"sync/atomic"
	"testing"

	"github.com/wzshiming/rdma/internal/rsproto"
)

// nativeOnMemLink is the native engine's stream on the memory link of
// rsproto.
var nativeOnMemLink = engineUnderTest{name: "native", new: newMemEngine, addr: "10.99.0.1:0", owns: memOwns}

// memEngine connects native streams over memory links of rsproto, each end
// on a memTransport. It fails the test when the memory of a stream is never
// released, or when its transport is processed after the release or by two
// goroutines at once.
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
		if misuse := t.misuseSeen(); misuse != "" {
			e.fail("the transport of a stream was %s", misuse)
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
	client.pollFor(cc.PollTime)
	server.pollFor(sc.PollTime)
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
// link of rsproto. A goroutine of its own plays the device of a queue pair:
// poked by the posts at either end, it takes in what reaches this end, the
// messages the peer sent, into the memory of the stream, and the completions
// of the sends the peer took, into two queues, one for each kind, and raises
// the event of a queue armed for what it took, which spends the arm. The
// stream gets what the queues hold only as it is processed: at its calls,
// after such an event, and at once on the loss of the link, as the real
// transport has it learned through the watcher.
type memTransport struct {
	end  *rsproto.MemEnd
	peer *memTransport
	mem  rsproto.Config
	kick chan struct{} // holds a poke not yet taken
	stop chan struct{} // closed by down
	done chan struct{} // closed when the device returns

	ev   linkEvents // set by start
	proc processor

	processes atomic.Int64                    // calls of process
	expected  [anyCompletion + 1]atomic.Int64 // calls of expect, by the kinds expected

	silence sync.Mutex // held, it keeps the device from taking anything, as a peer gone silent

	mu        sync.Mutex
	arrived   []rsproto.Msg // the messages taken in and not yet processed
	completed []memDone     // the completions taken in and not yet processed
	loss      error         // the loss of the link, once taken in
	lossFed   bool
	acks      int      // how many completions of sends the device may take in yet, -1 for all
	armed     interest // the queues that raise an event for the next completion they take
	arms      int      // how many times the queues were armed
	beforeArm func()   // run as the queues are next armed
	draining  bool
	misuse    string // what the transport was made to do that it never must
	started   bool
	released  bool

	feedArrived   []rsproto.Msg // the drain's
	feedCompleted []memDone     // the drain's
}

// memDone is the completion of a send as the device takes it in.
type memDone struct {
	id  uint64
	err error
}

func newMemTransport(end *rsproto.MemEnd, mem rsproto.Config) *memTransport {
	for _, r := range []rsproto.Region{mem.SendBuffer, mem.ReceiveBuffer, mem.GrantTable, mem.ControlBuffer} {
		end.Register(r)
	}
	// The queues hold no more than the receives and the sends posted.
	return &memTransport{
		end: end, mem: mem, acks: -1,
		kick: make(chan struct{}, 1), stop: make(chan struct{}), done: make(chan struct{}),
		arrived: make([]rsproto.Msg, 0, mem.ReceiveQueueSize), feedArrived: make([]rsproto.Msg, 0, mem.ReceiveQueueSize),
		completed: make([]memDone, 0, mem.SendQueueSize), feedCompleted: make([]memDone, 0, mem.SendQueueSize),
	}
}

// poke makes the device look at the end again.
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
	t.proc.start(t, ev)
	go t.device()
}

func (t *memTransport) process() {
	t.processes.Add(1)
	t.proc.process()
}

func (t *memTransport) expect(k interest) {
	t.expected[k].Add(1)
	t.proc.expect(k)
}

func (t *memTransport) device() {
	defer close(t.done)
	for {
		select {
		case <-t.kick:
		case <-t.stop:
			return
		}
		t.takeIn()
	}
}

// takeIn has the device take in what reached this end, raises the events of
// the queues armed for it and has the loss of the link processed at once.
func (t *memTransport) takeIn() {
	t.silence.Lock()
	fire, lost := t.land()
	t.silence.Unlock()
	if fire != 0 {
		t.proc.spent(fire)
	}
	if lost {
		t.proc.process()
	}
}

// land takes in what reached this end: the messages the peer sent, as many as
// there are receives, into the memory of the stream, which Deliver writes
// under the stream's lock, the completions of the sends the peer took, and
// the loss of the link once they are all in. It returns the kinds of what it
// took whose queues were armed, which it disarms, and whether the loss came.
func (t *memTransport) land() (fire interest, lost bool) {
	t.ev.lock()
	defer t.ev.unlock()
	t.mu.Lock()
	defer t.mu.Unlock()
	var took interest
	for msg, ok := t.end.Deliver(); ok; msg, ok = t.end.Deliver() {
		t.arrived = append(t.arrived, msg)
		t.peer.poke()
		took |= arrivals
	}
	for t.acks != 0 {
		id, err, ok := t.end.Completion()
		if !ok {
			break
		}
		t.completed = append(t.completed, memDone{id, err})
		took |= sendCompletions
		if t.acks > 0 {
			t.acks--
		}
	}
	if t.loss == nil {
		t.loss = t.end.Down()
		lost = t.loss != nil
	}
	fire = took & t.armed
	t.armed &^= fire
	return fire, lost
}

// drain hands ev what the queues hold, in the order the device took it in,
// and the loss of the link after it, as a processor drains a completion
// queue: taken from the queue first, then fed under ev's lock.
func (t *memTransport) drain(ev linkEvents) (interest, bool) {
	t.mu.Lock()
	t.check("drained")
	t.draining = true
	t.feedArrived, t.arrived = t.arrived, t.feedArrived[:0]
	t.feedCompleted, t.completed = t.completed, t.feedCompleted[:0]
	var loss error
	if t.loss != nil && !t.lossFed {
		loss, t.lossFed = t.loss, true
	}
	t.mu.Unlock()
	ev.lock()
	for _, msg := range t.feedArrived {
		ev.received(msg)
	}
	for _, d := range t.feedCompleted {
		ev.sent(d.id, d.err)
	}
	if loss != nil {
		ev.lost(loss)
	}
	k := ev.waitsFor()
	ev.unlock()
	t.mu.Lock()
	t.draining = false
	t.mu.Unlock()
	return k, true
}

// arm has the queues of the kinds k raise an event for the next completion
// they take, each queue on its own.
func (t *memTransport) arm(ev linkEvents, k interest) interest {
	t.mu.Lock()
	hook := t.beforeArm
	t.beforeArm = nil
	t.mu.Unlock()
	if hook != nil {
		hook()
	}
	t.mu.Lock()
	defer t.mu.Unlock()
	t.check("armed")
	t.armed |= k
	t.arms++
	return k
}

// check records the first time the queues are drained or armed when they
// never must be: after release, or while a drain goes on. The caller holds
// mu.
func (t *memTransport) check(what string) {
	switch {
	case t.misuse != "":
	case t.released:
		t.misuse = what + " after its release"
	case t.draining:
		t.misuse = what + " while another goroutine drained it"
	}
}

// down breaks the link, which both ends learn as its loss, stops the device
// and ends the processing.
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
	t.proc.stop()
}

// release poisons the memory, as freeing it would let it be reused, so that
// a view used after release shows.
func (t *memTransport) release() {
	t.mu.Lock()
	defer t.mu.Unlock()
	for _, r := range []rsproto.Region{t.mem.SendBuffer, t.mem.ReceiveBuffer, t.mem.GrantTable, t.mem.ControlBuffer} {
		for i := range r.Buf {
			r.Buf[i] = 0xdb
		}
	}
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

func (t *memTransport) misuseSeen() string {
	t.mu.Lock()
	defer t.mu.Unlock()
	return t.misuse
}

// holdAcks keeps the device from taking in the completions of sends, as if
// the acknowledgements of the peer were slow to come; ack then lets n more
// in, and releaseAcks all of them.
func (t *memTransport) holdAcks() { t.setAcks(0) }

func (t *memTransport) ack(n int) {
	t.mu.Lock()
	if t.acks >= 0 {
		t.acks += n
	}
	t.mu.Unlock()
	t.poke()
}

func (t *memTransport) releaseAcks() { t.setAcks(-1) }

func (t *memTransport) setAcks(n int) {
	t.mu.Lock()
	t.acks = n
	t.mu.Unlock()
	t.poke()
}

// isArmed reports whether the queues of the kinds k raise an event for the
// next completion they take.
func (t *memTransport) isArmed(k interest) bool {
	t.mu.Lock()
	defer t.mu.Unlock()
	return t.armed&k == k
}

// armCount returns how many times the queues were armed.
func (t *memTransport) armCount() int {
	t.mu.Lock()
	defer t.mu.Unlock()
	return t.arms
}

// processCount returns how many times the stream had the transport process.
func (t *memTransport) processCount() int64 { return t.processes.Load() }

// expectCount returns how many times the stream declared that it waits for
// the completions of the kinds k, exactly those.
func (t *memTransport) expectCount(k interest) int64 { return t.expected[k].Load() }

// beforeNextArm has f run as the queues are next armed, before the arm.
func (t *memTransport) beforeNextArm(f func()) {
	t.mu.Lock()
	defer t.mu.Unlock()
	t.beforeArm = f
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
