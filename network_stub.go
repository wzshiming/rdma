//go:build !linux || android || !(amd64 || arm64 || loong64 || ppc64le || riscv64 || s390x)

package rdma

import (
	"errors"

	"github.com/wzshiming/rdma/rsocket"
)

// platformEngine returns the engine c selects on a platform without
// rdma-core, which fails with an error matching errors.ErrUnsupported either
// way: the native engine for EngineNative, as no library it runs on can be
// loaded, the librdmacm engine otherwise, on a network whose socket fails, so
// nothing is ever polled.
func platformEngine(c Config) engine {
	if c.Engine == EngineNative {
		return nativeEngine{}
	}
	return librdmacmEngine{unsupportedNetwork{}}
}

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
