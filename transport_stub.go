//go:build !linux || !cgo

package rdma

import (
	"context"
	"errors"
	"net/netip"
)

// platformFabric returns the fabric of a platform without librdmacm: every
// connection attempt fails with errors.ErrUnsupported.
func platformFabric() fabric { return unsupportedFabric{} }

type unsupportedFabric struct{}

func (unsupportedFabric) dial(context.Context, netip.AddrPort, netip.AddrPort, Config, []byte) (transport, []byte, error) {
	return nil, nil, errors.ErrUnsupported
}

func (unsupportedFabric) listen(netip.AddrPort, int) (acceptor, error) {
	return nil, errors.ErrUnsupported
}
