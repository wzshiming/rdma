package rdma

import (
	"context"
	"errors"
	"net"
	"net/netip"
	"sync"
	"time"

	"github.com/wzshiming/rdma/internal/rsocket"
)

// socket is the non-blocking stream socket the librdmacm engine drives, with
// the shape of *rsocket.Socket: on Linux with librdmacm it is one, in tests
// an in-memory fake.
// A call that would block fails with an error matching errAgain, a Connect
// that goes on in the background with one matching errInProgress, and Read
// returns io.EOF once the peer has shut down writing.
type socket interface {
	// Descriptor returns the descriptor rsocket.Poll waits on; it is valid
	// until Close.
	Descriptor() int
	Bind(local netip.AddrPort) error
	Listen(backlog int) error
	// Accept returns the next connection with its peer address; the socket
	// is blocking until SetNonblock(true).
	Accept() (socket, netip.AddrPort, error)
	Connect(remote netip.AddrPort) error
	// SocketError reports how a Connect that reported errInProgress ended.
	SocketError() error
	Read(b []byte) (int, error)
	// Write writes what fits and returns how much, errAgain when nothing does.
	Write(b []byte) (int, error)
	Shutdown(how rsocket.How) error
	Close() error
	LocalAddr() (netip.AddrPort, error)
	RemoteAddr() (netip.AddrPort, error)
	SetNonblock(on bool) error
	SetSendBuffer(bytes int) error
	SetReceiveBuffer(bytes int) error
	SetSendQueueSize(n int) error
	SetReceiveQueueSize(n int) error
	SetInline(bytes int) error
}

// network is what the librdmacm engine runs on: it creates sockets, waits for
// their readiness and runs the calls on them. rsockets keep per-socket counters
// that rsend, rrecv and the completion processing of rpoll all update
// without a common lock, so once a socket is attached every call on it goes
// through run, which the poller executes between two polls, or happens while
// the socket is excluded from the poll. Interests are one-shot and the
// poller refers to a socket only between attach and detach, so that Close
// never races the poll.
type network interface {
	// socket creates a stream socket of the address family.
	socket(domain rsocket.Domain) (socket, error)
	// attach hands s to the poller; it precedes every wait on s.
	attach(s socket) error
	// run executes f, a call on s, on the poller's goroutine between two
	// polls, after include when s is excluded. It reports false, f not
	// run, when s is not attached.
	run(s socket, f func()) bool
	// exclude takes s out of the poll and holds its calls back until
	// include, returning once no poll refers to s, so that the caller may
	// make a call on s that blocks.
	exclude(s socket)
	// include ends an exclude.
	include(s socket)
	// wait registers a one-shot interest in events on s and returns the
	// channel that receives the events once they hold, errors and hang-ups
	// included, or rsocket.PollNval at once when s is not attached, and a
	// function that withdraws the interest.
	wait(s socket, events rsocket.Events) (ready <-chan rsocket.Events, cancel func())
	// detach removes s from the poller and returns once the poller no
	// longer refers to it, so that s may be closed; pending waits on s are
	// never answered and pending runs report false.
	detach(s socket)
}

// domain returns the address family of a, IPv6 unless it holds an IPv4
// address.
func domain(a *Addr) rsocket.Domain {
	if a.IP.To4() != nil {
		return rsocket.IPv4
	}
	return rsocket.IPv6
}

// apply sets every non-zero field on s, which must not be connected or
// listening yet.
func (c Config) apply(s socket) error {
	for _, o := range []struct {
		value int
		set   func(int) error
	}{
		{c.SendBufferSize, s.SetSendBuffer},
		{c.ReceiveBufferSize, s.SetReceiveBuffer},
		{c.SendQueueSize, s.SetSendQueueSize},
		{c.ReceiveQueueSize, s.SetReceiveQueueSize},
		{c.MaxInline, s.SetInline},
	} {
		if o.value != 0 {
			if err := o.set(o.value); err != nil {
				return err
			}
		}
	}
	return nil
}

// librdmacmEngine is the engine that carries the connections as rsockets of
// librdmacm, each in non-blocking mode and attached to the poller of nw.
type librdmacmEngine struct{ nw network }

func (e librdmacmEngine) dial(ctx context.Context, laddr, raddr *Addr, c Config) (stream, error) {
	s, err := e.nw.socket(domain(raddr))
	if err != nil {
		return nil, err
	}
	if err := c.apply(s); err != nil {
		s.Close()
		return nil, err
	}
	if laddr != nil {
		if err := s.Bind(laddr.addrPort()); err != nil {
			s.Close()
			return nil, err
		}
	}
	if err := s.SetNonblock(true); err != nil {
		s.Close()
		return nil, err
	}
	if err := e.nw.attach(s); err != nil {
		s.Close()
		return nil, err
	}
	if err := connect(ctx, e.nw, s, raddr); err != nil {
		e.nw.detach(s)
		s.Close()
		return nil, err
	}
	return newRstream(e.nw, s), nil
}

// connect starts a non-blocking connect to raddr and waits for its end, or
// for ctx, through the poller; s is attached.
func connect(ctx context.Context, nw network, s socket, raddr *Addr) error {
	var err error
	if !nw.run(s, func() { err = s.Connect(raddr.addrPort()) }) {
		return net.ErrClosed
	}
	if !errors.Is(err, errInProgress) {
		return err
	}
	ready, cancel := nw.wait(s, rsocket.PollOut)
	select {
	case <-ready:
		if !nw.run(s, func() { err = s.SocketError() }) {
			return net.ErrClosed
		}
		return err
	case <-ctx.Done():
		cancel()
		return ctx.Err()
	}
}

func (e librdmacmEngine) listen(laddr *Addr, backlog int, c Config) (acceptor, error) {
	s, err := e.nw.socket(domain(laddr))
	if err != nil {
		return nil, err
	}
	for _, step := range []func() error{
		func() error { return c.apply(s) },
		func() error { return s.Bind(laddr.addrPort()) },
		func() error { return s.Listen(backlog) },
		func() error { return s.SetNonblock(true) },
		func() error { return e.nw.attach(s) },
	} {
		if err := step(); err != nil {
			s.Close()
			return nil, err
		}
	}
	return &rlistener{nw: e.nw, s: s, closing: make(chan struct{})}, nil
}

// rlistener is a listening rsocket attached to the poller.
type rlistener struct {
	nw network
	s  socket

	mu      sync.Mutex
	closed  bool
	closing chan struct{} // closed by close, waking every accept
}

func (l *rlistener) addr() netip.AddrPort {
	a, err := l.s.LocalAddr()
	if err != nil {
		return netip.AddrPort{}
	}
	return a
}

func (l *rlistener) isClosed() bool {
	l.mu.Lock()
	defer l.mu.Unlock()
	return l.closed
}

func (l *rlistener) accept() (stream, error) {
	// rpoll sleeps on a listener's event channel, which a new request
	// stirs, but the librdmacm thread then completes the handshake and
	// queues the connection without waking pollers, so one interest stays
	// armed while the queue is asked again now and then.
	var ready <-chan rsocket.Events
	cancel := func() {}
	defer func() { cancel() }()
	retry := time.NewTimer(acceptRetry)
	defer retry.Stop()
	for {
		if l.isClosed() {
			return nil, net.ErrClosed
		}
		var s socket
		var err error
		if !l.nw.run(l.s, func() { s, _, err = l.s.Accept() }) {
			return nil, net.ErrClosed
		}
		if err == nil {
			// rsockets hand out blocking sockets whatever the listener is.
			if err := s.SetNonblock(true); err != nil {
				s.Close()
				return nil, err
			}
			if err := l.nw.attach(s); err != nil {
				s.Close()
				return nil, err
			}
			return newRstream(l.nw, s), nil
		}
		if !errors.Is(err, errAgain) {
			return nil, err
		}
		if ready == nil {
			ready, cancel = l.nw.wait(l.s, rsocket.PollIn)
		}
		retry.Reset(acceptRetry)
		select {
		case <-ready:
			ready = nil
		case <-retry.C:
		case <-l.closing:
			return nil, net.ErrClosed
		}
	}
}

// acceptRetry is how long accept lets the poller wait before it asks the
// listening socket again.
const acceptRetry = 10 * time.Millisecond

func (l *rlistener) close() error {
	l.mu.Lock()
	if l.closed {
		l.mu.Unlock()
		return nil
	}
	l.closed = true
	close(l.closing)
	l.mu.Unlock()
	l.nw.detach(l.s)
	return l.s.Close()
}

// rstream is a connected rsocket attached to the poller, through which every
// call on it goes.
type rstream struct {
	nw network
	s  socket

	mu      sync.Mutex
	closed  bool
	closing chan struct{} // closed by close, waking every wait
}

func newRstream(nw network, s socket) *rstream {
	return &rstream{nw: nw, s: s, closing: make(chan struct{})}
}

func (s *rstream) addrs() (local, remote netip.AddrPort) {
	s.nw.run(s.s, func() {
		if a, err := s.s.LocalAddr(); err == nil {
			local = a
		}
		if a, err := s.s.RemoteAddr(); err == nil {
			remote = a
		}
	})
	return local, remote
}

func (s *rstream) read(p []byte) (n int, err error) {
	if !s.nw.run(s.s, func() { n, err = s.s.Read(p) }) {
		return 0, net.ErrClosed
	}
	return n, err
}

func (s *rstream) write(p []byte) (n int, err error) {
	if !s.nw.run(s.s, func() { n, err = s.s.Write(p) }) {
		return 0, net.ErrClosed
	}
	return n, err
}

func (s *rstream) wait(dir direction, cancel <-chan struct{}) error {
	events := rsocket.PollIn
	if dir == writable {
		events = rsocket.PollOut
	}
	ready, stop := s.nw.wait(s.s, events)
	select {
	case <-ready:
		return nil
	case <-s.closing:
		stop()
		return net.ErrClosed
	case <-cancel:
		stop()
		return nil
	}
}

func (s *rstream) closeWrite() error {
	// Shutdown may block, so it runs here rather than on the poller, which
	// leaves the socket alone meanwhile.
	s.nw.exclude(s.s)
	err := s.s.Shutdown(rsocket.ShutWrite)
	s.nw.include(s.s)
	return err
}

func (s *rstream) close() error {
	s.mu.Lock()
	if s.closed {
		s.mu.Unlock()
		return nil
	}
	s.closed = true
	close(s.closing)
	s.mu.Unlock()
	// The poller must have let go of the descriptor before rclose frees it.
	s.nw.detach(s.s)
	return s.s.Close()
}
