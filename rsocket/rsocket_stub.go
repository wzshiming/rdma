//go:build !linux || !cgo

package rsocket

import (
	"errors"
	"net/netip"
	"time"

	"github.com/wzshiming/rdma/verbs"
)

func unsupported(op string) error { return &verbs.OpError{Op: op, Err: errors.ErrUnsupported} }

// Socket is a stream rsocket; librdmacm is unavailable on this platform.
type Socket struct{}

// New creates a stream socket of the address family domain.
func New(domain Domain) (*Socket, error) { return nil, unsupported("rsocket") }

// Descriptor returns the rsockets descriptor, -1 once closed.
func (s *Socket) Descriptor() int { return -1 }

// Bind binds the socket to a local address.
func (s *Socket) Bind(local netip.AddrPort) error { return unsupported("rbind") }

// Listen starts listening for connections.
func (s *Socket) Listen(backlog int) error { return unsupported("rlisten") }

// Accept takes the next connection.
func (s *Socket) Accept() (*Socket, netip.AddrPort, error) {
	return nil, netip.AddrPort{}, unsupported("raccept")
}

// Connect connects to remote.
func (s *Socket) Connect(remote netip.AddrPort) error { return unsupported("rconnect") }

// SocketError reports how an asynchronous Connect ended.
func (s *Socket) SocketError() error { return unsupported("rgetsockopt") }

// Read reads from the connection.
func (s *Socket) Read(b []byte) (int, error) { return 0, unsupported("rrecv") }

// Write writes to the connection.
func (s *Socket) Write(b []byte) (int, error) { return 0, unsupported("rsend") }

// Shutdown closes one or both halves of the connection.
func (s *Socket) Shutdown(how How) error { return unsupported("rshutdown") }

// Close releases the socket.
func (s *Socket) Close() error { return unsupported("rclose") }

// LocalAddr returns the local address of the socket.
func (s *Socket) LocalAddr() (netip.AddrPort, error) {
	return netip.AddrPort{}, unsupported("rgetsockname")
}

// RemoteAddr returns the address of the peer.
func (s *Socket) RemoteAddr() (netip.AddrPort, error) {
	return netip.AddrPort{}, unsupported("rgetpeername")
}

// SetSendBuffer sets the send buffer size (SO_SNDBUF).
func (s *Socket) SetSendBuffer(bytes int) error { return unsupported("rsetsockopt") }

// SetReceiveBuffer sets the receive buffer size (SO_RCVBUF).
func (s *Socket) SetReceiveBuffer(bytes int) error { return unsupported("rsetsockopt") }

// SetSendQueueSize sets the send queue depth (RDMA_SQSIZE).
func (s *Socket) SetSendQueueSize(n int) error { return unsupported("rsetsockopt") }

// SetReceiveQueueSize sets the receive queue depth (RDMA_RQSIZE).
func (s *Socket) SetReceiveQueueSize(n int) error { return unsupported("rsetsockopt") }

// SetInline sets the largest send posted inline (RDMA_INLINE).
func (s *Socket) SetInline(bytes int) error { return unsupported("rsetsockopt") }

// SetReuseAddr sets SO_REUSEADDR.
func (s *Socket) SetReuseAddr(on bool) error { return unsupported("rsetsockopt") }

// SetV6Only sets IPV6_V6ONLY.
func (s *Socket) SetV6Only(on bool) error { return unsupported("rsetsockopt") }

// SetNonblock sets or clears O_NONBLOCK.
func (s *Socket) SetNonblock(on bool) error { return unsupported("rfcntl") }

// Poll waits for events on rsockets and OS descriptors.
func Poll(fds []PollFD, timeout time.Duration) (int, error) { return 0, unsupported("rpoll") }
