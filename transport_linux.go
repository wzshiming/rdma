//go:build linux && cgo

package rdma

import (
	"context"
	"errors"
	"net/netip"
	"sync/atomic"
)

// registeredBytes counts the registered buffer memory of live connections.
var registeredBytes atomic.Int64

// platformFabric returns the fabric built on cm and verbs.
func platformFabric() fabric { return rcFabric{} }

type rcFabric struct{}

var errNotImplemented = errors.New("rdma: the verbs transport is not implemented yet")

func (rcFabric) dial(context.Context, netip.AddrPort, netip.AddrPort, Config, []byte) (transport, []byte, error) {
	return nil, nil, errNotImplemented
}

func (rcFabric) listen(netip.AddrPort, int) (acceptor, error) {
	return nil, errNotImplemented
}
