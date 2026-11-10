package rdma

import (
	"net/netip"

	"github.com/wzshiming/rdma/rsocket"
)

// socket is the non-blocking stream socket the engine drives, with the shape
// of *rsocket.Socket: on Linux with librdmacm it is one, in tests an
// in-memory fake.
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

// network is the platform seam: it creates sockets, waits for their
// readiness and runs the calls on them. rsockets keep per-socket counters
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

// rsock adapts *rsocket.Socket to the socket interface, whose Accept
// returns the interface.
type rsock struct{ *rsocket.Socket }

func (s *rsock) Accept() (socket, netip.AddrPort, error) {
	c, peer, err := s.Socket.Accept()
	if err != nil {
		return nil, netip.AddrPort{}, err
	}
	return &rsock{c}, peer, nil
}

// domain returns the address family of a, IPv6 unless it holds an IPv4
// address.
func domain(a *Addr) rsocket.Domain {
	if a.IP.To4() != nil {
		return rsocket.IPv4
	}
	return rsocket.IPv6
}
