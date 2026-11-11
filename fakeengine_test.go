package rdma

import (
	"context"
	"io"
	"net"
	"net/netip"
	"sync"
	"testing"
	"unsafe"
)

// fakeEngine is an in-memory engine with the manners the seam asks of a real
// one: calls other than wait never block, each direction holds a bounded
// buffer whose space returns to the writer only once the reader has consumed
// it, and close keeps a lent view valid until it is returned. Its lending
// form lends its send buffer and its receive ring through lender. It fails
// the test when the front end breaks a rule: a second view in one direction,
// a call in a direction whose view is out, a view never returned, buffers
// reclaimed while a view is out. A closed end's buffers are poisoned once no
// view of them is out.
type fakeEngine struct {
	fail  func(format string, args ...any)
	lends bool

	mu        sync.Mutex
	listeners map[int]*fakeAcceptor
	streams   []*fakeStream
	nextPort  int
}

func newLendingFake(t testing.TB) engine { return newFakeEngine(t, true) }

func newCopyingFake(t testing.TB) engine { return newFakeEngine(t, false) }

func newFakeEngine(t testing.TB, lends bool) *fakeEngine {
	e := &fakeEngine{
		fail: func(format string, args ...any) {
			t.Helper()
			t.Errorf("fake engine: "+format, args...)
		},
		lends:     lends,
		listeners: make(map[int]*fakeAcceptor),
		nextPort:  40000,
	}
	t.Cleanup(e.checkReclaimed)
	return e
}

// checkReclaimed fails the test for every stream whose buffers were not
// reclaimed: one left open, or closed with a view never returned.
func (e *fakeEngine) checkReclaimed() {
	e.mu.Lock()
	streams := e.streams
	e.mu.Unlock()
	for _, s := range streams {
		s.mu.Lock()
		if !s.reclaimed {
			e.fail("%v not reclaimed: closed %v, read view out %v, write view out %v", s.local, s.closed, s.rview != nil, s.wview != nil)
		}
		s.mu.Unlock()
	}
}

func (e *fakeEngine) dial(ctx context.Context, laddr, raddr *Addr, c Config) (stream, error) {
	e.mu.Lock()
	defer e.mu.Unlock()
	a := e.listeners[raddr.Port]
	if a == nil {
		return nil, errRefused
	}
	e.nextPort++
	client, server := e.connect(c, a.cfg, netip.AddrPortFrom(fakeIP, uint16(e.nextPort)), raddr.addrPort())
	select {
	case a.pending <- server:
	default:
		return nil, errRefused
	}
	return e.wrap(client), nil
}

func (e *fakeEngine) listen(laddr *Addr, backlog int, c Config) (acceptor, error) {
	e.mu.Lock()
	defer e.mu.Unlock()
	port := laddr.Port
	if port == 0 {
		e.nextPort++
		port = e.nextPort
	}
	if e.listeners[port] != nil {
		return nil, errAddrInUse
	}
	a := &fakeAcceptor{
		e:       e,
		bound:   netip.AddrPortFrom(laddr.addrPort().Addr(), uint16(port)),
		cfg:     c,
		pending: make(chan *fakeStream, 16),
		closing: make(chan struct{}),
	}
	e.listeners[port] = a
	return a, nil
}

// connect creates the two ends of a connection between the addresses, each
// with the buffers its Config asks for. The caller holds e.mu.
func (e *fakeEngine) connect(cc, sc Config, caddr, saddr netip.AddrPort) (client, server *fakeStream) {
	mu := new(sync.Mutex)
	client = &fakeStream{e: e, mu: mu, local: caddr, remote: saddr, closing: make(chan struct{})}
	server = &fakeStream{e: e, mu: mu, local: saddr, remote: caddr, closing: make(chan struct{})}
	client.peer, server.peer = server, client
	for _, x := range []struct {
		s *fakeStream
		c Config
	}{{client, cc}, {server, sc}} {
		x.s.sbuf = make([]byte, sizeOr(x.c.SendBufferSize, fakeRcvbuf))
		x.s.in = &fakeRing{ring: make([]byte, sizeOr(x.c.ReceiveBufferSize, fakeRcvbuf))}
	}
	client.out, server.out = server.in, client.in
	e.streams = append(e.streams, client, server)
	return client, server
}

func sizeOr(size, def int) int {
	if size > 0 {
		return size
	}
	return def
}

// wrap returns s in the engine's form.
func (e *fakeEngine) wrap(s *fakeStream) stream {
	if e.lends {
		return &fakeLendingStream{s}
	}
	return s
}

type fakeAcceptor struct {
	e       *fakeEngine
	bound   netip.AddrPort
	cfg     Config
	pending chan *fakeStream
	closing chan struct{}
	once    sync.Once
}

func (a *fakeAcceptor) addr() netip.AddrPort { return a.bound }

func (a *fakeAcceptor) accept() (stream, error) {
	select {
	case <-a.closing:
		return nil, net.ErrClosed
	default:
	}
	select {
	case s := <-a.pending:
		return a.e.wrap(s), nil
	case <-a.closing:
		return nil, net.ErrClosed
	}
}

func (a *fakeAcceptor) close() error {
	a.once.Do(func() {
		close(a.closing)
		a.e.mu.Lock()
		delete(a.e.listeners, int(a.bound.Port()))
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

// fakeRing is one direction of a connection: the receiver's ring, which
// holds n unread bytes from head on.
type fakeRing struct {
	ring    []byte
	head, n int
	eof     bool          // the writer will write no more
	rwake   chan struct{} // closed when the reader may proceed, nil unless awaited
	wwake   chan struct{} // closed when the writer may proceed, nil unless awaited
}

func (p *fakeRing) free() int { return len(p.ring) - p.n }

// put appends b, which fits, to the unread bytes.
func (p *fakeRing) put(b []byte) {
	tail := (p.head + p.n) % len(p.ring)
	k := copy(p.ring[tail:], b)
	copy(p.ring, b[k:])
	p.n += len(b)
	p.wakeReader()
}

// take moves unread bytes into b and returns how many.
func (p *fakeRing) take(b []byte) int {
	n := 0
	for n < len(b) && p.n > 0 {
		k := copy(b[n:], p.contiguous())
		p.consume(k)
		n += k
	}
	return n
}

// contiguous returns the unread bytes up to the end of the ring.
func (p *fakeRing) contiguous() []byte {
	end := min(p.head+p.n, len(p.ring))
	return p.ring[p.head:end:end]
}

// consume frees the first k unread bytes for the writer.
func (p *fakeRing) consume(k int) {
	p.head = (p.head + k) % len(p.ring)
	p.n -= k
	p.wakeWriter()
}

func (p *fakeRing) wakeReader() {
	if p.rwake != nil {
		close(p.rwake)
		p.rwake = nil
	}
}

func (p *fakeRing) wakeWriter() {
	if p.wwake != nil {
		close(p.wwake)
		p.wwake = nil
	}
}

// fakeStream is one end of a connection of the fake engine; in is its own
// ring, out its peer's. Both ends share mu.
type fakeStream struct {
	e             *fakeEngine
	mu            *sync.Mutex
	peer          *fakeStream
	in, out       *fakeRing
	sbuf          []byte // the send buffer the write views lie in
	local, remote netip.AddrPort
	closing       chan struct{} // closed by close

	closed, writeShut bool
	rview, wview      []byte // the views out, nil when none
	reclaimed         bool
}

func (s *fakeStream) addrs() (local, remote netip.AddrPort) { return s.local, s.remote }

func (s *fakeStream) read(p []byte) (int, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.rview != nil {
		s.e.fail("read on %v with a read view out", s.local)
	}
	switch {
	case s.closed:
		return 0, net.ErrClosed
	case len(p) == 0:
		return 0, nil
	case s.in.n == 0 && s.in.eof:
		return 0, io.EOF
	case s.in.n == 0:
		return 0, errAgain
	}
	return s.in.take(p), nil
}

func (s *fakeStream) write(p []byte) (int, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.wview != nil {
		s.e.fail("write on %v with a write view out", s.local)
	}
	if err := s.writeError(); err != nil {
		return 0, err
	}
	n := min(len(p), s.out.free())
	if n == 0 && len(p) > 0 {
		return 0, errAgain
	}
	s.out.put(p[:n])
	return n, nil
}

// writeError returns why nothing can be written, if anything does. The
// caller holds mu.
func (s *fakeStream) writeError() error {
	switch {
	case s.closed:
		return net.ErrClosed
	case s.writeShut:
		return errPipe
	case s.peer.closed:
		return errConnReset
	}
	return nil
}

// wait blocks until s is ready in dir. Shutting down writing does not wake a
// wait for room, as rsockets' rpoll need not either.
func (s *fakeStream) wait(dir direction, cancel <-chan struct{}) error {
	s.mu.Lock()
	if s.closed {
		s.mu.Unlock()
		return net.ErrClosed
	}
	var ch chan struct{}
	if dir == readable {
		if s.in.n > 0 || s.in.eof {
			s.mu.Unlock()
			return nil
		}
		if s.in.rwake == nil {
			s.in.rwake = make(chan struct{})
		}
		ch = s.in.rwake
	} else {
		if s.out.free() > 0 || s.writeError() != nil {
			s.mu.Unlock()
			return nil
		}
		if s.out.wwake == nil {
			s.out.wwake = make(chan struct{})
		}
		ch = s.out.wwake
	}
	s.mu.Unlock()
	select {
	case <-ch:
		return nil
	case <-cancel:
		return nil
	case <-s.closing:
		return net.ErrClosed
	}
}

func (s *fakeStream) closeWrite() error {
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.closed {
		return net.ErrClosed
	}
	s.writeShut = true
	s.out.eof = true
	s.out.wakeReader()
	return nil
}

func (s *fakeStream) close() error {
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.closed {
		return nil
	}
	s.closed = true
	close(s.closing)
	s.out.eof = true
	s.out.wakeReader()
	s.in.wakeWriter()
	s.reclaimIdle()
	return nil
}

// reclaimIdle poisons the buffers of a closed s once no view of them is out,
// as an engine would free them. The caller holds mu.
func (s *fakeStream) reclaimIdle() {
	if !s.closed || s.rview != nil || s.wview != nil {
		return
	}
	for _, b := range [][]byte{s.sbuf, s.in.ring} {
		for i := range b {
			b[i] = 0xdb
		}
	}
	s.reclaimed = true
}

// fakeLendingStream is a stream of the fake engine's lending form.
type fakeLendingStream struct{ *fakeStream }

func (s *fakeLendingStream) lendRead() ([]byte, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.rview != nil {
		s.e.fail("second read view on %v", s.local)
	}
	switch {
	case s.closed:
		return nil, net.ErrClosed
	case s.in.n == 0 && s.in.eof:
		return nil, io.EOF
	case s.in.n == 0:
		return nil, errAgain
	}
	s.rview = s.in.contiguous()
	return s.rview, nil
}

func (s *fakeLendingStream) returnRead(n int) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.rview == nil || n < 0 || n > len(s.rview) {
		s.e.fail("read view of %d bytes on %v returned with %d consumed", len(s.rview), s.local, n)
		return errInvalid
	}
	s.rview = nil
	if s.closed {
		s.reclaimIdle()
		return net.ErrClosed
	}
	s.in.consume(n)
	return nil
}

func (s *fakeLendingStream) lendWrite() ([]byte, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.wview != nil {
		s.e.fail("second write view on %v", s.local)
	}
	if err := s.writeError(); err != nil {
		return nil, err
	}
	n := min(len(s.sbuf), s.out.free())
	if n == 0 {
		return nil, errAgain
	}
	s.wview = s.sbuf[:n:n]
	return s.wview, nil
}

func (s *fakeLendingStream) commitWrite(n int) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.wview == nil || n < 0 || n > len(s.wview) {
		s.e.fail("write view of %d bytes on %v committed with %d filled", len(s.wview), s.local, n)
		return errInvalid
	}
	view := s.wview
	s.wview = nil
	if err := s.writeError(); err != nil {
		s.reclaimIdle()
		return err
	}
	s.out.put(view[:n])
	return nil
}

// fakeOwns reports whether p lies in the send buffer or the receive ring of
// s, a stream of the fake engine.
func fakeOwns(s stream, p []byte) bool {
	var fs *fakeStream
	switch s := s.(type) {
	case *fakeStream:
		fs = s
	case *fakeLendingStream:
		fs = s.fakeStream
	default:
		return false
	}
	return within(p, fs.sbuf) || within(p, fs.in.ring)
}

// within reports whether p, not empty, lies in b.
func within(p, b []byte) bool {
	if len(p) == 0 || len(b) == 0 {
		return false
	}
	ps, bs := uintptr(unsafe.Pointer(unsafe.SliceData(p))), uintptr(unsafe.Pointer(unsafe.SliceData(b)))
	return ps >= bs && ps+uintptr(len(p)) <= bs+uintptr(len(b))
}
