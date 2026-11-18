//go:build !linux || android || !(amd64 || arm64 || loong64 || ppc64le || riscv64 || s390x)

package cm

import (
	"context"
	"net/netip"

	"github.com/wzshiming/rdma/verbs"
)

// DatagramID is a communication identifier of the datagram service; the
// platform has no RDMA connection manager.
type DatagramID struct{}

// ResolveDatagram resolves the address and route to remote for an
// identifier of the datagram service.
func ResolveDatagram(ctx context.Context, local, remote netip.AddrPort) (*DatagramID, error) {
	return Dialer{}.ResolveDatagram(ctx, local, remote)
}

// ResolveDatagram resolves the address and route to remote for an
// identifier of the datagram service with the options of d.
func (d Dialer) ResolveDatagram(ctx context.Context, local, remote netip.AddrPort) (*DatagramID, error) {
	if err := checkResolveAddrs(local, remote); err != nil {
		return nil, err
	}
	if err := d.options().checkDatagram(); err != nil {
		return nil, err
	}
	return nil, unsupported("rdma_resolve_addr")
}

// BindDatagram binds an identifier of the datagram service to local.
func BindDatagram(local netip.AddrPort) (*DatagramID, error) {
	if err := checkBindAddr(local); err != nil {
		return nil, err
	}
	return nil, unsupported("rdma_bind_addr")
}

// Device returns the device the identifier uses.
func (id *DatagramID) Device() *verbs.Context { return nil }

// Port returns the device port the identifier uses.
func (id *DatagramID) Port() uint8 { return 0 }

// LocalAddr returns the local address of the identifier.
func (id *DatagramID) LocalAddr() netip.AddrPort { return netip.AddrPort{} }

// RemoteAddr returns the address of the peer.
func (id *DatagramID) RemoteAddr() netip.AddrPort { return netip.AddrPort{} }

// CreateQP creates the queue pair of the identifier.
func (id *DatagramID) CreateQP(pd *verbs.PD, attr *verbs.QPInitAttr) (*verbs.QP, error) {
	return nil, unsupported("rdma_create_qp")
}

// QP returns the queue pair created by CreateQP.
func (id *DatagramID) QP() *verbs.QP { return nil }

// Close destroys the queue pair and the identifier.
func (id *DatagramID) Close() error { return nil }

// DatagramListener listens for the requests of the datagram service; the
// platform has no RDMA connection manager.
type DatagramListener struct{}

// ListenDatagram listens for the requests of the datagram service on local.
func ListenDatagram(local netip.AddrPort, backlog int) (*DatagramListener, error) {
	return ListenConfig{}.ListenDatagram(local, backlog)
}

// ListenDatagram listens for the requests of the datagram service on local
// with the options of lc.
func (lc ListenConfig) ListenDatagram(local netip.AddrPort, backlog int) (*DatagramListener, error) {
	if err := checkListenAddr(local); err != nil {
		return nil, err
	}
	if err := lc.options().checkDatagram(); err != nil {
		return nil, err
	}
	return nil, unsupported("rdma_listen")
}

// Addr returns the bound address.
func (l *DatagramListener) Addr() netip.AddrPort { return netip.AddrPort{} }

// Accept waits for the next request.
func (l *DatagramListener) Accept(ctx context.Context) (*DatagramRequest, error) {
	return nil, unsupported("rdma_get_cm_event")
}

// Close stops listening.
func (l *DatagramListener) Close() error { return nil }
