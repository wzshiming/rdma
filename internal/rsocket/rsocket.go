package rsocket

import "strconv"

// Domain is the address family of a socket.
type Domain int

// Address families of New.
const (
	IPv4 Domain = iota // AF_INET
	IPv6               // AF_INET6
)

var domainNames = []string{"AF_INET", "AF_INET6"}

func (d Domain) String() string { return enumString("Domain", int(d), domainNames) }

// How names the halves of a connection Socket.Shutdown closes.
type How int

// Shutdown modes.
const (
	ShutRead      How = iota // SHUT_RD: no more receiving
	ShutWrite                // SHUT_WR: no more sending, the peer reads EOF
	ShutReadWrite            // SHUT_RDWR
)

var howNames = []string{"SHUT_RD", "SHUT_WR", "SHUT_RDWR"}

func (h How) String() string { return enumString("How", int(h), howNames) }

// Events is a set of poll(2) events, requested in PollFD.Events and reported
// in PollFD.Revents.
type Events uint16

// Poll events, with the values of poll(2).
const (
	PollIn   Events = 0x001 // POLLIN: data, a connection request or EOF to read
	PollOut  Events = 0x004 // POLLOUT: room to write, or a connect attempt ended
	PollErr  Events = 0x008 // POLLERR: an error, read with Socket.SocketError
	PollHup  Events = 0x010 // POLLHUP: the peer disconnected
	PollNval Events = 0x020 // POLLNVAL: not an open descriptor
)

var eventNames = []struct {
	bit  Events
	name string
}{
	{PollIn, "POLLIN"}, {PollOut, "POLLOUT"}, {PollErr, "POLLERR"}, {PollHup, "POLLHUP"}, {PollNval, "POLLNVAL"},
}

func (e Events) String() string {
	if e == 0 {
		return "0"
	}
	var s string
	for _, n := range eventNames {
		if e&n.bit != 0 {
			if s != "" {
				s += "|"
			}
			s += n.name
			e &^= n.bit
		}
	}
	if e != 0 {
		if s != "" {
			s += "|"
		}
		s += "0x" + strconv.FormatUint(uint64(e), 16)
	}
	return s
}

// PollFD is one descriptor Poll waits on: an rsockets descriptor from
// Socket.Descriptor or a plain OS file descriptor such as a pipe or eventfd.
type PollFD struct {
	Fd      int
	Events  Events // events to wait for
	Revents Events // events that occurred, filled in by Poll
}

func enumString(typ string, v int, names []string) string {
	if v >= 0 && v < len(names) {
		return names[v]
	}
	return typ + "(" + strconv.Itoa(v) + ")"
}
