package rdma

import (
	"context"
	"net"
	"time"
)

// Dialer contains options for connecting to an address, like net.Dialer. The
// zero value is ready to use.
type Dialer struct {
	// Timeout is the maximum amount of time a dial waits for a connection
	// to complete; zero means no timeout. The shorter of Timeout, Deadline
	// and the context's deadline applies.
	Timeout time.Duration
	// Deadline is the absolute point in time after which dials fail; zero
	// means no deadline.
	Deadline time.Time
	// LocalAddr is the local address, an *Addr, to dial from; its IP picks
	// the device and a non-zero port the local port. nil lets rdma_cm pick.
	LocalAddr net.Addr
	// Config sets the rsockets options of the connection.
	Config Config

	eng engine // tests inject an engine; nil selects the platform's
}

// Dial connects to the address on the named network, which must be "rdma",
// "rdma4" or "rdma6", with the zero Dialer.
//
// The address is host:port; the host is an IP address or a name resolved
// with net.DefaultResolver, and the local device is chosen by rdma_cm for
// the route to it. The peer may be any rsockets application. Errors are
// *net.OpError values: errors.Is reports syscall.ECONNREFUSED when nobody
// listens on the port, the context's error when it ends first, and
// errors.ErrUnsupported on platforms without rdma-core, when its libraries
// cannot be loaded, and on an iWARP device.
func Dial(network, address string) (*Conn, error) {
	var d Dialer
	return d.DialContext(context.Background(), network, address)
}

// DialContext connects to the address on the named network using the
// provided context and the zero Dialer.
func DialContext(ctx context.Context, network, address string) (*Conn, error) {
	var d Dialer
	return d.DialContext(ctx, network, address)
}

// Dial connects to the address on the named network. See func Dial for a
// description of the network and address parameters.
func (d *Dialer) Dial(network, address string) (*Conn, error) {
	return d.DialContext(context.Background(), network, address)
}

// DialContext connects to the address on the named network using the
// provided context. Once the connection is established the context no
// longer affects it.
func (d *Dialer) DialContext(ctx context.Context, network, address string) (*Conn, error) {
	ctx, cancel := d.withDeadline(ctx)
	defer cancel()
	raddr, err := resolve(ctx, network, address)
	if err != nil {
		return nil, &net.OpError{Op: "dial", Net: "rdma", Err: err}
	}
	fail := func(err error) (*Conn, error) {
		return nil, &net.OpError{Op: "dial", Net: "rdma", Source: d.LocalAddr, Addr: raddr, Err: err}
	}
	if len(raddr.IP) == 0 {
		return fail(&net.AddrError{Err: "missing IP address", Addr: address})
	}
	var laddr *Addr
	if d.LocalAddr != nil {
		var ok bool
		if laddr, ok = d.LocalAddr.(*Addr); !ok {
			return fail(&net.AddrError{Err: "mismatched local address type", Addr: d.LocalAddr.String()})
		}
	}
	if err := d.Config.validate(); err != nil {
		return fail(err)
	}
	s, err := d.engine().dial(ctx, laddr, raddr, d.Config)
	if err != nil {
		return fail(err)
	}
	return newConn(s), nil
}

// withDeadline bounds ctx by the Dialer's Timeout and Deadline; the error of
// a dial that runs out of time is context.DeadlineExceeded, whose Timeout
// method reports true.
func (d *Dialer) withDeadline(ctx context.Context) (context.Context, context.CancelFunc) {
	deadline := d.Deadline
	if d.Timeout > 0 {
		if t := time.Now().Add(d.Timeout); deadline.IsZero() || t.Before(deadline) {
			deadline = t
		}
	}
	if deadline.IsZero() {
		return context.WithCancel(ctx)
	}
	return context.WithDeadline(ctx, deadline)
}

func (d *Dialer) engine() engine {
	if d.eng != nil {
		return d.eng
	}
	return platformEngine()
}
