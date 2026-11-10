package rdma

import (
	"context"
	"errors"
	"net"
	"os"
	"sync"
	"time"

	"github.com/wzshiming/rdma/rsocket"
)

// ListenConfig contains options for listening to an address, like
// net.ListenConfig. The zero value is ready to use.
type ListenConfig struct {
	// Config sets the rsockets options of the listening socket, which the
	// accepted connections inherit.
	Config Config
	// Backlog bounds the connection requests waiting to be accepted; 0
	// means the kernel's default.
	Backlog int

	nw network // tests inject a network; nil selects the platform's
}

// Listen announces on the local network address with the zero ListenConfig.
//
// The network must be "rdma", "rdma4" or "rdma6" and the address host:port;
// the host is an IP address that picks the device, a name resolved with
// net.DefaultResolver, or empty for every device, and port 0 picks a free
// port, which Listener.Addr reports. Errors are *net.OpError values.
func Listen(network, address string) (*Listener, error) {
	var lc ListenConfig
	return lc.Listen(context.Background(), network, address)
}

// Listen announces on the local network address; ctx bounds the address
// resolution only. See func Listen for the network and address parameters.
func (lc *ListenConfig) Listen(ctx context.Context, network, address string) (*Listener, error) {
	addr, err := resolve(ctx, network, address)
	if err != nil {
		return nil, &net.OpError{Op: "listen", Net: "rdma", Err: err}
	}
	if len(addr.IP) == 0 {
		addr.IP = net.IPv4zero
		if network == "rdma6" {
			addr.IP = net.IPv6unspecified
		}
	}
	fail := func(err error) (*Listener, error) {
		return nil, &net.OpError{Op: "listen", Net: "rdma", Addr: addr, Err: err}
	}
	if err := lc.Config.validate(); err != nil {
		return fail(err)
	}
	nw := lc.network()
	s, err := nw.socket(domain(addr))
	if err != nil {
		return fail(err)
	}
	for _, step := range []func() error{
		func() error { return lc.Config.apply(s) },
		func() error { return s.Bind(addr.addrPort()) },
		func() error { return s.Listen(lc.Backlog) },
		func() error { return s.SetNonblock(true) },
		func() error { return nw.attach(s) },
	} {
		if err := step(); err != nil {
			s.Close()
			return fail(err)
		}
	}
	l := &Listener{nw: nw, s: s, addr: addr, closing: make(chan struct{})}
	if a, err := s.LocalAddr(); err == nil {
		if bound := addrFromAddrPort(a); bound != nil {
			l.addr = bound
		}
	}
	return l, nil
}

func (lc *ListenConfig) network() network {
	if lc.nw != nil {
		return lc.nw
	}
	return platformNetwork()
}

// Listener is an RDMA stream listener. It implements net.Listener.
type Listener struct {
	nw   network
	s    socket
	addr *Addr

	mu      sync.Mutex
	closed  bool
	closing chan struct{} // closed by Close, waking every Accept
}

func (l *Listener) opError(op string, err error) error {
	if errors.Is(err, os.ErrClosed) {
		err = net.ErrClosed
	}
	return &net.OpError{Op: op, Net: "rdma", Addr: l.addr, Err: err}
}

func (l *Listener) isClosed() bool {
	l.mu.Lock()
	defer l.mu.Unlock()
	return l.closed
}

// Accept waits for and returns the next connection to the listener. The
// returned net.Conn is a *Conn.
func (l *Listener) Accept() (net.Conn, error) {
	c, err := l.AcceptRDMA()
	if err != nil {
		return nil, err
	}
	return c, nil
}

// AcceptRDMA waits for and returns the next connection to the listener.
// After Close it returns an error matching net.ErrClosed.
func (l *Listener) AcceptRDMA() (*Conn, error) {
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
			return nil, l.opError("accept", net.ErrClosed)
		}
		var s socket
		var err error
		if !l.nw.run(l.s, func() { s, _, err = l.s.Accept() }) {
			return nil, l.opError("accept", net.ErrClosed)
		}
		if err == nil {
			// rsockets hand out blocking sockets whatever the listener is.
			if err := s.SetNonblock(true); err != nil {
				s.Close()
				return nil, l.opError("accept", err)
			}
			if err := l.nw.attach(s); err != nil {
				s.Close()
				return nil, l.opError("accept", err)
			}
			return newConn(l.nw, s), nil
		}
		if !errors.Is(err, errAgain) {
			return nil, l.opError("accept", err)
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
			return nil, l.opError("accept", net.ErrClosed)
		}
	}
}

// acceptRetry is how long Accept lets the poller wait before it asks the
// listening socket again.
const acceptRetry = 10 * time.Millisecond

// Close stops listening; a blocked Accept returns an error matching
// net.ErrClosed. Established connections are not affected.
func (l *Listener) Close() error {
	l.mu.Lock()
	if l.closed {
		l.mu.Unlock()
		return l.opError("close", net.ErrClosed)
	}
	l.closed = true
	close(l.closing)
	l.mu.Unlock()
	l.nw.detach(l.s)
	if err := l.s.Close(); err != nil {
		return l.opError("close", err)
	}
	return nil
}

// Addr returns the listener's address, an *Addr with the port actually
// bound.
func (l *Listener) Addr() net.Addr { return l.addr }
