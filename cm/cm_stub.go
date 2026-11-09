//go:build !linux || !cgo

package cm

import (
	"context"
	"errors"
	"net/netip"

	"github.com/wzshiming/rdma/verbs"
)

func unsupported(op string) error { return &verbs.OpError{Op: op, Err: errors.ErrUnsupported} }

// ID is a communication identifier; librdmacm is unavailable on this platform.
type ID struct{}

// Resolve resolves the address and route to remote.
func Resolve(ctx context.Context, local, remote netip.AddrPort) (*ID, error) {
	if err := checkResolveAddrs(local, remote); err != nil {
		return nil, err
	}
	return nil, unsupported("rdma_resolve_addr")
}

// Device returns the device the connection uses.
func (id *ID) Device() *verbs.Context { return nil }

// Port returns the device port the connection uses.
func (id *ID) Port() uint8 { return 0 }

// LocalAddr returns the local address of the connection.
func (id *ID) LocalAddr() netip.AddrPort { return netip.AddrPort{} }

// RemoteAddr returns the address of the peer.
func (id *ID) RemoteAddr() netip.AddrPort { return netip.AddrPort{} }

// CreateQP creates the queue pair of the connection.
func (id *ID) CreateQP(pd *verbs.PD, attr *verbs.QPInitAttr) (*verbs.QP, error) {
	return nil, unsupported("rdma_create_qp")
}

// QP returns the queue pair created by CreateQP.
func (id *ID) QP() *verbs.QP { return nil }

// Connect connects to the peer.
func (id *ID) Connect(ctx context.Context, param *ConnParam) (*ConnInfo, error) {
	if err := checkConnParam("rdma_connect", param, MaxConnectPrivateData); err != nil {
		return nil, err
	}
	return nil, unsupported("rdma_connect")
}

// Disconnect disconnects from the peer.
func (id *ID) Disconnect(ctx context.Context) error { return unsupported("rdma_disconnect") }

// Wait waits for the next event of the connection.
func (id *ID) Wait(ctx context.Context) (Event, error) { return 0, unsupported("rdma_get_cm_event") }

// Close destroys the queue pair and the identifier.
func (id *ID) Close() error { return nil }

// Listener accepts connection requests; librdmacm is unavailable on this
// platform.
type Listener struct{}

// Listen listens for connection requests on local.
func Listen(local netip.AddrPort, backlog int) (*Listener, error) {
	if err := checkListenAddr(local); err != nil {
		return nil, err
	}
	return nil, unsupported("rdma_listen")
}

// Addr returns the bound address.
func (l *Listener) Addr() netip.AddrPort { return netip.AddrPort{} }

// Accept waits for the next connection request.
func (l *Listener) Accept(ctx context.Context) (*Request, error) {
	return nil, unsupported("rdma_get_cm_event")
}

// Close stops listening.
func (l *Listener) Close() error { return nil }

// Accept accepts the request.
func (r *Request) Accept(ctx context.Context, param *ConnParam) error {
	if err := checkConnParam("rdma_accept", param, MaxAcceptPrivateData); err != nil {
		return err
	}
	return unsupported("rdma_accept")
}

// Reject rejects the request.
func (r *Request) Reject(privateData []byte) error {
	if err := checkPrivateData("rdma_reject", privateData, MaxRejectPrivateData); err != nil {
		return err
	}
	return unsupported("rdma_reject")
}
