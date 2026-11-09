package rdma

import (
	"context"
	"errors"
	"fmt"
	"net"
	"time"

	"github.com/wzshiming/rdma/cm"
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
	// Config tunes the connection's buffers; message size and receive depth
	// are negotiated down with the listener.
	Config Config

	fab fabric // tests inject a fabric; nil selects the platform's
}

// Dial connects to the address on the named network, which must be "rdma",
// "rdma4" or "rdma6", with the zero Dialer. See func Dial for the address
// syntax.
//
// The address is host:port; the host is an IP address or a name resolved
// with net.DefaultResolver, and the local device is chosen by rdma_cm for
// the route to it. Errors are *net.OpError values: errors.Is reports
// syscall.ECONNREFUSED when nobody listens on the port, ErrProtocol when the
// listener does not speak this protocol version, the context's error when
// it ends first, and errors.ErrUnsupported on platforms without librdmacm.
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
	cfg := d.Config.withDefaults()
	if err := cfg.validate(); err != nil {
		return fail(err)
	}
	t, reply, err := d.fabric().dial(ctx, laddr.addrPort(), raddr.addrPort(), cfg, encodeHello(cfg))
	if err != nil {
		var rej *cm.RejectedError
		if errors.As(err, &rej) {
			err = rejectedError(rej)
		}
		return fail(err)
	}
	negotiated, err := acceptReply(cfg, reply)
	if err != nil {
		t.close()
		return fail(err)
	}
	return newConn(t, negotiated), nil
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

func (d *Dialer) fabric() fabric {
	if d.fab != nil {
		return d.fab
	}
	return platformFabric()
}

// acceptReply checks the listener's reply against our proposal and returns
// the negotiated configuration.
func acceptReply(cfg Config, reply []byte) (Config, error) {
	h, err := decodeHello(reply)
	if err != nil {
		return Config{}, err
	}
	switch {
	case h.version != protocolVersion:
		return Config{}, fmt.Errorf("%w: peer replied with protocol version %d, want %d", ErrProtocol, h.version, protocolVersion)
	case h.messageSize < minMessageSize || int(h.messageSize) > cfg.MessageSize:
		return Config{}, fmt.Errorf("%w: peer replied with message size %d after a proposal of %d", ErrProtocol, h.messageSize, cfg.MessageSize)
	case h.receiveDepth < 1 || int(h.receiveDepth) > cfg.ReceiveDepth:
		return Config{}, fmt.Errorf("%w: peer replied with receive depth %d after a proposal of %d", ErrProtocol, h.receiveDepth, cfg.ReceiveDepth)
	}
	cfg.MessageSize = int(h.messageSize)
	cfg.ReceiveDepth = int(h.receiveDepth)
	return cfg, nil
}
