package rdma

import (
	"context"
	"errors"
	"net"
	"os"
	"sync"
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
	a, err := librdmacmEngine{lc.network()}.listen(addr, lc.Backlog, lc.Config)
	if err != nil {
		return fail(err)
	}
	l := &Listener{a: a, addr: addr}
	if bound := addrFromAddrPort(a.addr()); bound != nil {
		l.addr = bound
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
	a    *rlistener
	addr *Addr

	mu     sync.Mutex
	closed bool
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
	if l.isClosed() {
		return nil, l.opError("accept", net.ErrClosed)
	}
	s, err := l.a.accept()
	if err != nil {
		return nil, l.opError("accept", err)
	}
	return newConn(s), nil
}

// Close stops listening; a blocked Accept returns an error matching
// net.ErrClosed. Established connections are not affected.
func (l *Listener) Close() error {
	l.mu.Lock()
	if l.closed {
		l.mu.Unlock()
		return l.opError("close", net.ErrClosed)
	}
	l.closed = true
	l.mu.Unlock()
	if err := l.a.close(); err != nil {
		return l.opError("close", err)
	}
	return nil
}

// Addr returns the listener's address, an *Addr with the port actually
// bound.
func (l *Listener) Addr() net.Addr { return l.addr }
