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
	// Config tunes the buffers of accepted connections; message size and
	// receive depth are negotiated down with each dialer.
	Config Config
	// Backlog bounds the connection requests waiting to be accepted; 0
	// means the kernel's default.
	Backlog int

	fab fabric // tests inject a fabric; nil selects the platform's
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
	cfg := lc.Config.withDefaults()
	if err := cfg.validate(); err != nil {
		return fail(err)
	}
	fab := lc.fab
	if fab == nil {
		fab = platformFabric()
	}
	acc, err := fab.listen(addr.addrPort(), lc.Backlog)
	if err != nil {
		return fail(err)
	}
	l := &Listener{acc: acc, addr: addrFromAddrPort(acc.addr()), cfg: cfg}
	if l.addr == nil {
		l.addr = addr
	}
	l.ctx, l.cancel = context.WithCancel(context.Background())
	return l, nil
}

// Listener is an RDMA stream listener. It implements net.Listener.
type Listener struct {
	acc  acceptor
	addr *Addr
	cfg  Config

	ctx    context.Context // ends when the Listener closes
	cancel context.CancelFunc

	mu     sync.Mutex
	closed bool
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

// AcceptRDMA waits for and returns the next connection to the listener. A
// dialer that does not speak this protocol version is rejected and never
// returned; one that disappears during the handshake is dropped silently.
// After Close it returns an error matching net.ErrClosed.
func (l *Listener) AcceptRDMA() (*Conn, error) {
	for {
		req, err := l.acc.accept(l.ctx)
		if err != nil {
			if l.ctx.Err() != nil || errors.Is(err, net.ErrClosed) || errors.Is(err, os.ErrClosed) {
				err = net.ErrClosed
			}
			return nil, &net.OpError{Op: "accept", Net: "rdma", Addr: l.addr, Err: err}
		}
		h, err := decodeHello(req.hello())
		if err != nil {
			req.reject(encodeReject(rejectMalformed))
			continue
		}
		cfg, err := negotiate(l.cfg, h)
		if err != nil {
			code := rejectMalformed
			var rej *rejection
			if errors.As(err, &rej) {
				code = rej.code
			}
			req.reject(encodeReject(code))
			continue
		}
		t, err := req.accept(l.ctx, cfg, encodeHello(cfg))
		if err != nil {
			if l.ctx.Err() != nil {
				return nil, &net.OpError{Op: "accept", Net: "rdma", Addr: l.addr, Err: net.ErrClosed}
			}
			continue
		}
		return newConn(t, cfg), nil
	}
}

// Close stops listening; a blocked Accept returns an error matching
// net.ErrClosed. Established connections are not affected.
func (l *Listener) Close() error {
	l.mu.Lock()
	defer l.mu.Unlock()
	if l.closed {
		return &net.OpError{Op: "close", Net: "rdma", Addr: l.addr, Err: net.ErrClosed}
	}
	l.closed = true
	l.cancel()
	if err := l.acc.close(); err != nil {
		return &net.OpError{Op: "close", Net: "rdma", Addr: l.addr, Err: err}
	}
	return nil
}

// Addr returns the listener's address, an *Addr with the port actually
// bound.
func (l *Listener) Addr() net.Addr { return l.addr }
