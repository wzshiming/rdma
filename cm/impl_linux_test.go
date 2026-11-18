//go:build linux && !android && (amd64 || arm64 || loong64 || ppc64le || riscv64 || s390x)

package cm_test

import (
	"context"
	"net/netip"
	"testing"

	"github.com/wzshiming/rdma/cm"
	"github.com/wzshiming/rdma/internal/rdmacm"
	"github.com/wzshiming/rdma/verbs"
)

// impl is an implementation of the connection manager the device tests run
// on, seen through this package's types whichever it is.
type impl struct {
	name    string
	listen  func(local netip.AddrPort, backlog int) (listener, error)
	resolve func(ctx context.Context, local, remote netip.AddrPort) (conn, error)
	// listenWith and resolveWith create the identifier with the options of
	// a ListenConfig or a Dialer.
	listenWith  func(lc cm.ListenConfig, local netip.AddrPort, backlog int) (listener, error)
	resolveWith func(ctx context.Context, d cm.Dialer, local, remote netip.AddrPort) (conn, error)
}

// underTest is the implementation the tests examine, this package, the one
// place that names it; reference is librdmacm's through the binding of
// internal/rdmacm, which the tests compare it with.
var (
	underTest = impl{"cm", listenCM, resolveCM, listenConfigCM, dialerCM}
	reference = impl{"librdmacm", listenReference, resolveReference, listenConfigReference, dialerReference}
	impls     = []impl{underTest, reference}
)

// forEachImpl runs test on each implementation, as a subtest named after it.
func forEachImpl(t *testing.T, test func(t *testing.T, im impl)) {
	for _, im := range impls {
		t.Run(im.name, func(t *testing.T) { test(t, im) })
	}
}

// forEachPair runs test on each pair of the implementation of the connecting
// end and that of the listening end, as a subtest named after them.
func forEachPair(t *testing.T, test func(t *testing.T, active, passive impl)) {
	for _, active := range impls {
		for _, passive := range impls {
			t.Run(active.name+" to "+passive.name, func(t *testing.T) { test(t, active, passive) })
		}
	}
}

// conn is a connection's identifier: a *cm.ID, or the reference's.
type conn interface {
	Close() error
	Connect(ctx context.Context, param *cm.ConnParam) (*cm.ConnInfo, error)
	CreateQP(pd *verbs.PD, attr *verbs.QPInitAttr) (*verbs.QP, error)
	Device() *verbs.Context
	Disconnect(ctx context.Context) error
	LocalAddr() netip.AddrPort
	Port() uint8
	QP() *verbs.QP
	RemoteAddr() netip.AddrPort
	Wait(ctx context.Context) (cm.Event, error)
}

// listener is a *cm.Listener, or the reference's.
type listener interface {
	Accept(ctx context.Context) (*request, error)
	Addr() netip.AddrPort
	Close() error
}

// request is a connection request of either implementation, with the fields
// and methods of a cm.Request.
type request struct {
	ID                 conn
	PrivateData        []byte
	ResponderResources uint8
	InitiatorDepth     uint8

	accept func(ctx context.Context, param *cm.ConnParam) error
	reject func(privateData []byte) error
}

func (r *request) Accept(ctx context.Context, param *cm.ConnParam) error {
	return r.accept(ctx, param)
}

func (r *request) Reject(privateData []byte) error { return r.reject(privateData) }

func listenCM(local netip.AddrPort, backlog int) (listener, error) {
	l, err := cm.Listen(local, backlog)
	if err != nil {
		return nil, err
	}
	return cmListener{l}, nil
}

func resolveCM(ctx context.Context, local, remote netip.AddrPort) (conn, error) {
	id, err := cm.Resolve(ctx, local, remote)
	if err != nil {
		return nil, err
	}
	return id, nil
}

func listenConfigCM(lc cm.ListenConfig, local netip.AddrPort, backlog int) (listener, error) {
	l, err := lc.Listen(local, backlog)
	if err != nil {
		return nil, err
	}
	return cmListener{l}, nil
}

func dialerCM(ctx context.Context, d cm.Dialer, local, remote netip.AddrPort) (conn, error) {
	id, err := d.Resolve(ctx, local, remote)
	if err != nil {
		return nil, err
	}
	return id, nil
}

type cmListener struct{ *cm.Listener }

func (l cmListener) Accept(ctx context.Context) (*request, error) {
	req, err := l.Listener.Accept(ctx)
	if err != nil {
		return nil, err
	}
	return &request{
		ID:                 req.ID,
		PrivateData:        req.PrivateData,
		ResponderResources: req.ResponderResources,
		InitiatorDepth:     req.InitiatorDepth,
		accept:             req.Accept,
		reject:             req.Reject,
	}, nil
}

func listenReference(local netip.AddrPort, backlog int) (listener, error) {
	l, err := rdmacm.Listen(local, backlog)
	if err != nil {
		return nil, err
	}
	return referenceListener{l}, nil
}

func resolveReference(ctx context.Context, local, remote netip.AddrPort) (conn, error) {
	id, err := rdmacm.Resolve(ctx, local, remote)
	if err != nil {
		return nil, err
	}
	return referenceConn{id}, nil
}

func listenConfigReference(lc cm.ListenConfig, local netip.AddrPort, backlog int) (listener, error) {
	l, err := rdmacm.ListenConfig(lc).Listen(local, backlog)
	if err != nil {
		return nil, err
	}
	return referenceListener{l}, nil
}

func dialerReference(ctx context.Context, d cm.Dialer, local, remote netip.AddrPort) (conn, error) {
	id, err := rdmacm.Dialer(d).Resolve(ctx, local, remote)
	if err != nil {
		return nil, err
	}
	return referenceConn{id}, nil
}

type referenceListener struct{ *rdmacm.Listener }

func (l referenceListener) Accept(ctx context.Context) (*request, error) {
	req, err := l.Listener.Accept(ctx)
	if err != nil {
		return nil, err
	}
	return &request{
		ID:                 referenceConn{req.ID},
		PrivateData:        req.PrivateData,
		ResponderResources: req.ResponderResources,
		InitiatorDepth:     req.InitiatorDepth,
		accept: func(ctx context.Context, param *cm.ConnParam) error {
			return fromReference(req.Accept(ctx, (*rdmacm.ConnParam)(param)))
		},
		reject: req.Reject,
	}, nil
}

// referenceConn is an identifier of the reference taking and returning this
// package's types.
type referenceConn struct{ *rdmacm.ID }

func (c referenceConn) Connect(ctx context.Context, param *cm.ConnParam) (*cm.ConnInfo, error) {
	info, err := c.ID.Connect(ctx, (*rdmacm.ConnParam)(param))
	if err != nil {
		return nil, fromReference(err)
	}
	return (*cm.ConnInfo)(info), nil
}

func (c referenceConn) Wait(ctx context.Context) (cm.Event, error) {
	ev, err := c.ID.Wait(ctx)
	return cm.Event(ev), err
}

// fromReference turns a *rdmacm.RejectedError, which the reference returns
// unwrapped, into the *cm.RejectedError with its fields; the other errors of
// the two packages are of types they share.
func fromReference(err error) error {
	if rej, ok := err.(*rdmacm.RejectedError); ok {
		return (*cm.RejectedError)(rej)
	}
	return err
}
