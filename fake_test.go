package rdma

import (
	"fmt"
	"io"
	"net/netip"
	"os"
	"sync"
	"testing"

	"github.com/wzshiming/rdma/rsocket"
	"github.com/wzshiming/rdma/verbs"
)

// fakeNet is an in-memory network with the shape and the rules of rsockets
// that the engine has to respect: sockets are non-blocking and report EAGAIN,
// readiness comes through the network's waiting seam, a connect to a
// listener completes in the library before Accept collects it and wakes no
// waiter of the listener, a connect to an unknown host never ends, an
// accepted socket is blocking until told otherwise, buffers are bounded, and
// the space a reader frees reaches the writer only through a call that
// services the reader's socket, as rsockets return credits. It fails the
// test on a blocking call and on a socket closed while the poller still
// holds it.
type fakeNet struct {
	fail func(format string, args ...any)

	mu        sync.Mutex
	listeners map[netip.AddrPort]*fakeSocket
	attached  map[*fakeSocket]bool
	open      int // sockets created and not yet closed
	nextPort  uint16
	nextFD    int
	rcvbuf    int // receive buffer capacity of a socket without SetReceiveBuffer
}

// fakeIP is the address of the one host of the fake network; dials to any
// other address go unanswered.
var fakeIP = netip.MustParseAddr("10.99.0.1")

// fakeNowhere is an address nobody answers.
const fakeNowhere = "10.99.0.99:1"

// fakeRcvbuf is the default receive buffer: small enough for back-pressure
// to show up in every test that moves more than a few KiB.
const fakeRcvbuf = 64 << 10

func newFakeNet(t testing.TB) *fakeNet {
	return &fakeNet{
		fail: func(format string, args ...any) {
			t.Helper()
			t.Errorf("fake network: "+format, args...)
		},
		listeners: make(map[netip.AddrPort]*fakeSocket),
		attached:  make(map[*fakeSocket]bool),
		nextPort:  40000,
		rcvbuf:    fakeRcvbuf,
	}
}

type fakeState int

const (
	fakeInit fakeState = iota
	fakeListening
	fakeConnecting
	fakeConnected
	fakeClosed
)

// fakeOptions records the Set calls, 0 meaning not set.
type fakeOptions struct {
	sndbuf, rcvbuf, sqsize, rqsize, inline int
}

type fakeSocket struct {
	n      *fakeNet
	fd     int
	domain rsocket.Domain

	// Everything below is guarded by n.mu.
	state         fakeState
	nonblock      bool
	local, remote netip.AddrPort
	opts          fakeOptions

	pending []*fakeSocket // listening: dialers waiting to be accepted

	connDone bool  // connecting: the attempt ended
	connErr  error // connecting: how it failed

	peer      *fakeSocket
	rbuf      []byte // received, not yet read
	rcap      int    // capacity of rbuf
	freed     int    // read from rbuf but not yet credited to the peer
	readEOF   bool   // the peer shut down writing or closed
	writeShut bool

	waiters []*fakeWaiter
}

type fakeWaiter struct {
	events rsocket.Events
	ch     chan rsocket.Events
}

func (s *fakeSocket) String() string { return fmt.Sprintf("fake socket %d", s.fd) }

func (n *fakeNet) port() uint16 {
	n.nextPort++
	return n.nextPort
}

func (n *fakeNet) socket(domain rsocket.Domain) (socket, error) {
	n.mu.Lock()
	defer n.mu.Unlock()
	n.nextFD++
	n.open++
	return &fakeSocket{n: n, fd: n.nextFD, domain: domain, rcap: n.rcvbuf}, nil
}

func (n *fakeNet) attach(s socket) error {
	n.mu.Lock()
	defer n.mu.Unlock()
	fs := s.(*fakeSocket)
	if fs.state == fakeClosed {
		n.fail("%v attached after Close", fs)
	}
	n.attached[fs] = true
	return nil
}

func (n *fakeNet) wait(s socket, events rsocket.Events) (<-chan rsocket.Events, func()) {
	n.mu.Lock()
	defer n.mu.Unlock()
	fs := s.(*fakeSocket)
	ch := make(chan rsocket.Events, 1)
	if !n.attached[fs] {
		ch <- rsocket.PollNval
		return ch, func() {}
	}
	fs.service()
	if re := fs.ready(events); re != 0 {
		ch <- re
		return ch, func() {}
	}
	w := &fakeWaiter{events: events, ch: ch}
	fs.waiters = append(fs.waiters, w)
	return ch, func() {
		n.mu.Lock()
		defer n.mu.Unlock()
		fs.removeWaiter(w)
	}
}

func (n *fakeNet) detach(s socket) {
	n.mu.Lock()
	defer n.mu.Unlock()
	fs := s.(*fakeSocket)
	delete(n.attached, fs)
	fs.waiters = nil
}

// lookup finds the listener for remote, exact or wildcard. The caller holds
// n.mu.
func (n *fakeNet) lookup(remote netip.AddrPort) *fakeSocket {
	if l := n.listeners[remote]; l != nil {
		return l
	}
	for addr, l := range n.listeners {
		if addr.Addr().IsUnspecified() && addr.Port() == remote.Port() {
			return l
		}
	}
	return nil
}

// ready reports the events of want that hold, plus errors and hang-ups
// whether wanted or not, like rpoll. The caller holds n.mu.
func (s *fakeSocket) ready(want rsocket.Events) rsocket.Events {
	var re rsocket.Events
	switch s.state {
	case fakeClosed:
		re |= rsocket.PollNval
	case fakeListening:
		if len(s.pending) > 0 {
			re |= rsocket.PollIn
		}
	case fakeConnecting:
		if s.connDone {
			re |= rsocket.PollOut
			if s.connErr != nil {
				re |= rsocket.PollErr
			}
		}
	case fakeConnected:
		if len(s.rbuf) > 0 || s.readEOF {
			re |= rsocket.PollIn
		}
		if s.writeShut || s.peer.state == fakeClosed || s.peer.room() > 0 {
			re |= rsocket.PollOut
		}
		if s.peer.state == fakeClosed {
			re |= rsocket.PollHup
		}
	}
	return re&^(rsocket.PollIn|rsocket.PollOut) | re&want
}

// room is the space the peer may still write into s: what the peer has not
// filled, less what s read without crediting it back yet.
func (s *fakeSocket) room() int { return s.rcap - len(s.rbuf) - s.freed }

// service does what rsockets do in a call that services the socket: it hands
// the space freed by Read back to the peer. The caller holds n.mu.
func (s *fakeSocket) service() {
	if s.freed > 0 {
		s.freed = 0
		if s.peer != nil {
			s.peer.notify()
		}
	}
}

// notify delivers readiness to the waiters whose events hold. The caller
// holds n.mu.
func (s *fakeSocket) notify() {
	kept := s.waiters[:0]
	for _, w := range s.waiters {
		if re := s.ready(w.events); re != 0 {
			w.ch <- re
		} else {
			kept = append(kept, w)
		}
	}
	s.waiters = kept
}

func (s *fakeSocket) removeWaiter(w *fakeWaiter) {
	for i, x := range s.waiters {
		if x == w {
			s.waiters = append(s.waiters[:i], s.waiters[i+1:]...)
			return
		}
	}
}

// errno wraps an errno like the rsocket package does.
func errno(op string, err error) error { return &verbs.OpError{Op: op, Err: err} }

// errClosedSocket reports op on a closed socket like the rsocket package.
func errClosedSocket(op string) error { return &verbs.OpError{Op: op, Err: os.ErrClosed} }

// wouldBlock checks that a call that may block was made on a non-blocking
// socket, which the engine must have asked for. The caller holds n.mu.
func (s *fakeSocket) wouldBlock(op string) {
	if !s.nonblock {
		s.n.fail("%s on %v, which is blocking: SetNonblock(true) is missing", op, s)
	}
}

func (s *fakeSocket) Descriptor() int { return s.fd }

func (s *fakeSocket) Bind(local netip.AddrPort) error {
	s.n.mu.Lock()
	defer s.n.mu.Unlock()
	if s.state != fakeInit || s.local.IsValid() {
		return errno("rbind", errInvalid)
	}
	if local.Port() == 0 {
		local = netip.AddrPortFrom(local.Addr(), s.n.port())
	}
	s.local = local
	return nil
}

func (s *fakeSocket) Listen(backlog int) error {
	s.n.mu.Lock()
	defer s.n.mu.Unlock()
	if s.state != fakeInit {
		return errno("rlisten", errInvalid)
	}
	if !s.local.IsValid() {
		s.local = netip.AddrPortFrom(netip.IPv4Unspecified(), s.n.port())
	}
	if _, taken := s.n.listeners[s.local]; taken {
		return errno("rbind", errAddrInUse)
	}
	s.n.listeners[s.local] = s
	s.state = fakeListening
	return nil
}

func (s *fakeSocket) Accept() (socket, netip.AddrPort, error) {
	s.n.mu.Lock()
	defer s.n.mu.Unlock()
	if s.state != fakeListening {
		return nil, netip.AddrPort{}, errno("raccept", errInvalid)
	}
	s.wouldBlock("Accept")
	if len(s.pending) == 0 {
		return nil, netip.AddrPort{}, errno("raccept", errAgain)
	}
	server := s.pending[0]
	s.pending = s.pending[1:]
	return server, server.remote, nil
}

// accepted creates the connected socket a request to the listener s gets,
// as the librdmacm thread does before Accept is called. Options are
// inherited, the non-blocking flag is not. The caller holds n.mu.
func (s *fakeSocket) accepted(client *fakeSocket) *fakeSocket {
	local := s.local
	if local.Addr().IsUnspecified() {
		local = netip.AddrPortFrom(fakeIP, local.Port())
	}
	s.n.nextFD++
	s.n.open++
	server := &fakeSocket{n: s.n, fd: s.n.nextFD, domain: s.domain, state: fakeConnected, local: local, remote: client.local, opts: s.opts, rcap: s.rcap}
	server.peer, client.peer = client, server
	return server
}

func (s *fakeSocket) Connect(remote netip.AddrPort) error {
	s.n.mu.Lock()
	defer s.n.mu.Unlock()
	if s.state != fakeInit {
		return errno("rconnect", errInvalid)
	}
	s.wouldBlock("Connect")
	if !s.local.IsValid() {
		s.local = netip.AddrPortFrom(fakeIP, s.n.port())
	}
	s.remote = remote
	s.state = fakeConnecting
	if remote.Addr() != fakeIP {
		// No host answers: the attempt goes on for longer than any test
		// waits, as it does while rdma_cm retries an address resolution.
		return errno("rconnect", errInProgress)
	}
	l := s.n.lookup(remote)
	if l == nil {
		s.connDone = true
		s.connErr = errRefused
		return errno("rconnect", errInProgress)
	}
	// Like rsockets, whose librdmacm thread completes the handshake and
	// queues the connection without waking rpoll, the connect ends here
	// and the listener's waiters are not told.
	l.pending = append(l.pending, l.accepted(s))
	s.state = fakeConnected
	s.connDone = true
	return errno("rconnect", errInProgress)
}

func (s *fakeSocket) SocketError() error {
	s.n.mu.Lock()
	defer s.n.mu.Unlock()
	err := s.connErr
	s.connErr = nil
	if err != nil {
		return errno("rconnect", err)
	}
	return nil
}

func (s *fakeSocket) Read(b []byte) (int, error) {
	s.n.mu.Lock()
	defer s.n.mu.Unlock()
	if s.state == fakeClosed {
		return 0, errClosedSocket("rrecv")
	}
	if s.state != fakeConnected {
		return 0, errno("rrecv", errNotConn)
	}
	if len(b) == 0 {
		return 0, nil
	}
	s.wouldBlock("Read")
	if len(s.rbuf) == 0 {
		s.service()
		if s.readEOF {
			return 0, io.EOF
		}
		return 0, errno("rrecv", errAgain)
	}
	n := copy(b, s.rbuf)
	s.rbuf = s.rbuf[n:]
	s.freed += n
	return n, nil
}

func (s *fakeSocket) Write(b []byte) (int, error) {
	s.n.mu.Lock()
	defer s.n.mu.Unlock()
	if s.state == fakeClosed {
		return 0, errClosedSocket("rsend")
	}
	if s.state != fakeConnected {
		return 0, errno("rsend", errNotConn)
	}
	if len(b) == 0 {
		return 0, nil
	}
	s.wouldBlock("Write")
	s.service()
	if s.writeShut || s.peer.state == fakeClosed {
		return 0, errno("rsend", errConnReset)
	}
	room := s.peer.room()
	if room <= 0 {
		return 0, errno("rsend", errAgain)
	}
	n := min(room, len(b))
	s.peer.rbuf = append(s.peer.rbuf, b[:n]...)
	s.peer.notify()
	return n, nil
}

func (s *fakeSocket) Shutdown(how rsocket.How) error {
	s.n.mu.Lock()
	defer s.n.mu.Unlock()
	if s.state == fakeClosed {
		return errClosedSocket("rshutdown")
	}
	if s.state != fakeConnected {
		return errno("rshutdown", errNotConn)
	}
	if how == rsocket.ShutWrite || how == rsocket.ShutReadWrite {
		s.writeShut = true
		s.peer.readEOF = true
		s.peer.notify()
		s.notify()
	}
	return nil
}

func (s *fakeSocket) Close() error {
	s.n.mu.Lock()
	defer s.n.mu.Unlock()
	if s.state == fakeClosed {
		return errClosedSocket("rclose")
	}
	if s.n.attached[s] {
		s.n.fail("%v closed while attached to the poller: Close must follow detach", s)
	}
	switch s.state {
	case fakeListening:
		delete(s.n.listeners, s.local)
		// Connections nobody collected go down with the listener.
		for _, server := range s.pending {
			server.state = fakeClosed
			s.n.open--
			server.peer.readEOF = true
			server.peer.notify()
		}
	case fakeConnected:
		s.peer.readEOF = true
		s.peer.notify()
	}
	s.state = fakeClosed
	s.n.open--
	s.notify()
	return nil
}

func (s *fakeSocket) LocalAddr() (netip.AddrPort, error) {
	s.n.mu.Lock()
	defer s.n.mu.Unlock()
	if s.state == fakeClosed {
		return netip.AddrPort{}, errClosedSocket("rgetsockname")
	}
	return s.local, nil
}

func (s *fakeSocket) RemoteAddr() (netip.AddrPort, error) {
	s.n.mu.Lock()
	defer s.n.mu.Unlock()
	if s.state == fakeClosed {
		return netip.AddrPort{}, errClosedSocket("rgetpeername")
	}
	return s.remote, nil
}

func (s *fakeSocket) SetNonblock(on bool) error {
	s.n.mu.Lock()
	defer s.n.mu.Unlock()
	s.nonblock = on
	return nil
}

func (s *fakeSocket) setOption(name string, field *int, value int) error {
	s.n.mu.Lock()
	defer s.n.mu.Unlock()
	if s.state != fakeInit {
		s.n.fail("%s(%d) on %v after Listen or Connect, when it has no effect", name, value, s)
	}
	if value < 0 {
		return errno("rsetsockopt", errInvalid)
	}
	*field = value
	return nil
}

func (s *fakeSocket) SetSendBuffer(bytes int) error {
	return s.setOption("SetSendBuffer", &s.opts.sndbuf, bytes)
}

func (s *fakeSocket) SetReceiveBuffer(bytes int) error {
	if err := s.setOption("SetReceiveBuffer", &s.opts.rcvbuf, bytes); err != nil {
		return err
	}
	s.n.mu.Lock()
	defer s.n.mu.Unlock()
	s.rcap = bytes
	return nil
}

func (s *fakeSocket) SetSendQueueSize(n int) error {
	return s.setOption("SetSendQueueSize", &s.opts.sqsize, n)
}

func (s *fakeSocket) SetReceiveQueueSize(n int) error {
	return s.setOption("SetReceiveQueueSize", &s.opts.rqsize, n)
}

func (s *fakeSocket) SetInline(bytes int) error {
	return s.setOption("SetInline", &s.opts.inline, bytes)
}

// options returns the options recorded on a socket of the fake network.
func (n *fakeNet) options(s socket) fakeOptions {
	n.mu.Lock()
	defer n.mu.Unlock()
	return s.(*fakeSocket).opts
}

// openSockets returns the sockets created and not yet closed.
func (n *fakeNet) openSockets() int {
	n.mu.Lock()
	defer n.mu.Unlock()
	return n.open
}

// attachedSockets returns the sockets the poller seam still holds.
func (n *fakeNet) attachedSockets() int {
	n.mu.Lock()
	defer n.mu.Unlock()
	return len(n.attached)
}
