//go:build !linux || android || !(amd64 || arm64 || loong64 || ppc64le || riscv64 || s390x)

package rsocket_test

import (
	"errors"
	"net/netip"
	"testing"

	"github.com/wzshiming/rdma/internal/rsocket"
	"github.com/wzshiming/rdma/verbs"
)

func TestStubUnsupported(t *testing.T) {
	addr := netip.MustParseAddrPort("10.200.0.1:7471")
	s := new(rsocket.Socket)
	tests := []struct {
		name string
		call func() error
	}{
		{"New", func() error { _, err := rsocket.New(rsocket.IPv4); return err }},
		{"Poll", func() error { _, err := rsocket.Poll([]rsocket.PollFD{{Fd: 0, Events: rsocket.PollIn}}, 0); return err }},
		{"Socket.Bind", func() error { return s.Bind(addr) }},
		{"Socket.Listen", func() error { return s.Listen(1) }},
		{"Socket.Accept", func() error { _, _, err := s.Accept(); return err }},
		{"Socket.Connect", func() error { return s.Connect(addr) }},
		{"Socket.SocketError", func() error { return s.SocketError() }},
		{"Socket.Read", func() error { _, err := s.Read(make([]byte, 1)); return err }},
		{"Socket.Write", func() error { _, err := s.Write([]byte{1}); return err }},
		{"Socket.Shutdown", func() error { return s.Shutdown(rsocket.ShutWrite) }},
		{"Socket.Close", func() error { return s.Close() }},
		{"Socket.LocalAddr", func() error { _, err := s.LocalAddr(); return err }},
		{"Socket.RemoteAddr", func() error { _, err := s.RemoteAddr(); return err }},
		{"Socket.SetSendBuffer", func() error { return s.SetSendBuffer(1 << 16) }},
		{"Socket.SetReceiveBuffer", func() error { return s.SetReceiveBuffer(1 << 16) }},
		{"Socket.SetSendQueueSize", func() error { return s.SetSendQueueSize(64) }},
		{"Socket.SetReceiveQueueSize", func() error { return s.SetReceiveQueueSize(64) }},
		{"Socket.SetInline", func() error { return s.SetInline(64) }},
		{"Socket.SetReuseAddr", func() error { return s.SetReuseAddr(true) }},
		{"Socket.SetV6Only", func() error { return s.SetV6Only(true) }},
		{"Socket.SetNonblock", func() error { return s.SetNonblock(true) }},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			err := tt.call()
			if !errors.Is(err, errors.ErrUnsupported) {
				t.Fatalf("err = %v, want errors.ErrUnsupported", err)
			}
			var opErr *verbs.OpError
			if !errors.As(err, &opErr) || opErr.Op == "" {
				t.Fatalf("err = %T, want *verbs.OpError naming the rsockets function", err)
			}
		})
	}
	if fd := s.Descriptor(); fd != -1 {
		t.Errorf("stub Descriptor() = %d, want -1", fd)
	}
}
