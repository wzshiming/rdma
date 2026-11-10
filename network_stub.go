//go:build !linux || android || !(amd64 || arm64 || loong64 || ppc64le || riscv64 || s390x)

package rdma

import (
	"errors"

	"github.com/wzshiming/rdma/rsocket"
)

// platformNetwork returns the network of a platform without librdmacm:
// socket fails with errors.ErrUnsupported, so nothing is ever polled.
func platformNetwork() network { return unsupportedNetwork{} }

type unsupportedNetwork struct{}

func (unsupportedNetwork) socket(domain rsocket.Domain) (socket, error) {
	s, err := rsocket.New(domain)
	if err != nil {
		return nil, err
	}
	return &rsock{s}, nil
}

func (unsupportedNetwork) attach(socket) error { return errors.ErrUnsupported }

func (unsupportedNetwork) run(socket, func()) bool { return false }

func (unsupportedNetwork) exclude(socket) {}

func (unsupportedNetwork) include(socket) {}

func (unsupportedNetwork) wait(socket, rsocket.Events) (<-chan rsocket.Events, func()) {
	ch := make(chan rsocket.Events, 1)
	ch <- rsocket.PollNval
	return ch, func() {}
}

func (unsupportedNetwork) detach(socket) {}
