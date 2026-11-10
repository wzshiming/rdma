//go:build linux && cgo

package rsocket

/*
#cgo LDFLAGS: -lrdmacm
#include <rdma/rsocket.h>
*/
import "C"

import (
	"errors"
	"net/netip"
	"time"
)

var errNotImplemented = errors.New("rsocket: not implemented")

// Socket is a stream rsocket (rsocket(7)): a connected byte stream carried
// over an RDMA queue pair by librdmacm.
type Socket struct{}

// New creates a stream socket of the address family domain (rsocket with
// SOCK_STREAM). It fails with a *verbs.OpError carrying the errno of
// librdmacm on a host without an RDMA device or the rdma_cm module.
func New(domain Domain) (*Socket, error) { return nil, errNotImplemented }

// Descriptor returns the rsockets descriptor for Poll and diagnostics, or -1
// once the Socket is closed. It is not a kernel file descriptor: only the
// functions of this package may use it.
func (s *Socket) Descriptor() int { return -1 }

// Bind binds the socket to local (rbind); its IP picks the RDMA device and
// port 0 picks a free port.
func (s *Socket) Bind(local netip.AddrPort) error { return errNotImplemented }

// Listen starts listening for connections (rlisten); backlog bounds the
// requests waiting to be accepted.
func (s *Socket) Listen(backlog int) error { return errNotImplemented }

// Accept takes the next connection (raccept) and returns it with the peer's
// address. In non-blocking mode it fails with syscall.EAGAIN when no request
// is pending. The accepted Socket inherits the buffer, queue and inline
// options of the listener but not its non-blocking flag: call SetNonblock.
func (s *Socket) Accept() (*Socket, netip.AddrPort, error) {
	return nil, netip.AddrPort{}, errNotImplemented
}

// Connect connects to remote (rconnect). In non-blocking mode it fails with
// syscall.EINPROGRESS at once and the attempt goes on as Poll, Read and
// Write are called: Poll reports PollOut when it ends, with PollErr when it
// failed, and SocketError tells how.
func (s *Socket) Connect(remote netip.AddrPort) error { return errNotImplemented }

// SocketError reads and clears SO_ERROR, the result of an asynchronous
// Connect: nil for success, otherwise a *verbs.OpError with Op "rconnect" and
// the errno.
func (s *Socket) SocketError() error { return errNotImplemented }

// Read reads from the connection (rrecv). It returns 0, io.EOF once the peer
// has shut down writing or closed and the buffered data is read, and in
// non-blocking mode an error matching syscall.EAGAIN when no data is ready.
func (s *Socket) Read(b []byte) (int, error) { return 0, errNotImplemented }

// Write writes to the connection (rsend). It may write less than len(b) with
// a nil error, and in non-blocking mode returns 0 and an error matching
// syscall.EAGAIN when no buffer space is free.
func (s *Socket) Write(b []byte) (int, error) { return 0, errNotImplemented }

// Shutdown closes one or both halves of the connection (rshutdown); the peer
// reads EOF after ShutWrite. It waits for the posted sends to complete.
func (s *Socket) Shutdown(how How) error { return errNotImplemented }

// Close releases the socket (rclose) after waiting for the calls in progress
// on it and for its posted sends to complete. Every later call, Close
// included, fails with a *verbs.OpError wrapping os.ErrClosed, and the
// descriptor is never handed to librdmacm again.
func (s *Socket) Close() error { return errNotImplemented }

// LocalAddr returns the local address of the socket (rgetsockname).
func (s *Socket) LocalAddr() (netip.AddrPort, error) { return netip.AddrPort{}, errNotImplemented }

// RemoteAddr returns the address of the peer (rgetpeername).
func (s *Socket) RemoteAddr() (netip.AddrPort, error) { return netip.AddrPort{}, errNotImplemented }

// SetSendBuffer sets the send buffer size (SO_SNDBUF); rsockets doubles it.
func (s *Socket) SetSendBuffer(bytes int) error { return errNotImplemented }

// SetReceiveBuffer sets the receive buffer size (SO_RCVBUF); rsockets
// doubles it.
func (s *Socket) SetReceiveBuffer(bytes int) error { return errNotImplemented }

// SetSendQueueSize sets the send queue depth (RDMA_SQSIZE).
func (s *Socket) SetSendQueueSize(n int) error { return errNotImplemented }

// SetReceiveQueueSize sets the receive queue depth (RDMA_RQSIZE).
func (s *Socket) SetReceiveQueueSize(n int) error { return errNotImplemented }

// SetInline sets the largest send posted inline (RDMA_INLINE).
func (s *Socket) SetInline(bytes int) error { return errNotImplemented }

// SetReuseAddr sets SO_REUSEADDR; call it before Bind.
func (s *Socket) SetReuseAddr(on bool) error { return errNotImplemented }

// SetV6Only sets IPV6_V6ONLY; call it before Listen.
func (s *Socket) SetV6Only(on bool) error { return errNotImplemented }

// SetNonblock sets or clears O_NONBLOCK (rfcntl F_GETFL/F_SETFL).
func (s *Socket) SetNonblock(on bool) error { return errNotImplemented }

// Poll waits until an event of fds occurs or timeout passes (rpoll), fills
// in the Revents and returns the number of descriptors with events, 0 on
// timeout. A negative timeout waits without limit and 0 returns at once; an
// EINTR is retried for the time left. fds mixes rsockets descriptors and
// plain OS file descriptors freely and must not be empty.
func Poll(fds []PollFD, timeout time.Duration) (int, error) { return 0, errNotImplemented }
