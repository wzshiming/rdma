//go:build linux && cgo

package rsocket

/*
#cgo LDFLAGS: -lrdmacm
#include <errno.h>
#include <fcntl.h>
#include <poll.h>
#include <stdint.h>
#include <string.h>
#include <arpa/inet.h>
#include <netinet/in.h>
#include <sys/socket.h>
#include <rdma/rsocket.h>

// rfcntl is variadic, which cgo cannot call.
static int rs_getfl(int fd) { return rfcntl(fd, F_GETFL); }
static int rs_setfl(int fd, int flags) { return rfcntl(fd, F_SETFL, flags); }

static int rs_setsockopt_int(int fd, int level, int name, int value) {
	return rsetsockopt(fd, level, name, &value, sizeof value);
}

static int rs_getsockopt_int(int fd, int level, int name, int *value) {
	socklen_t len = sizeof *value;
	return rgetsockopt(fd, level, name, value, &len);
}

// rs_sockaddr fills ss with an IPv4 (4 bytes) or IPv6 (16 bytes) address and
// a host-order port and returns the length of the sockaddr.
static socklen_t rs_sockaddr(struct sockaddr_storage *ss, int family, const unsigned char *addr, uint16_t port) {
	memset(ss, 0, sizeof *ss);
	if (family == AF_INET) {
		struct sockaddr_in *sin = (struct sockaddr_in *)ss;
		sin->sin_family = AF_INET;
		sin->sin_port = htons(port);
		memcpy(&sin->sin_addr, addr, 4);
		return sizeof *sin;
	}
	struct sockaddr_in6 *sin6 = (struct sockaddr_in6 *)ss;
	sin6->sin6_family = AF_INET6;
	sin6->sin6_port = htons(port);
	memcpy(&sin6->sin6_addr, addr, 16);
	return sizeof *sin6;
}

// rs_addrport reads the address bytes and host-order port of an IPv4 or IPv6
// sockaddr into addr (16 bytes) and port and returns its family.
static int rs_addrport(const struct sockaddr_storage *ss, unsigned char *addr, uint16_t *port) {
	if (ss->ss_family == AF_INET) {
		const struct sockaddr_in *sin = (const struct sockaddr_in *)ss;
		memcpy(addr, &sin->sin_addr, 4);
		*port = ntohs(sin->sin_port);
	} else if (ss->ss_family == AF_INET6) {
		const struct sockaddr_in6 *sin6 = (const struct sockaddr_in6 *)ss;
		memcpy(addr, &sin6->sin6_addr, 16);
		*port = ntohs(sin6->sin6_port);
	}
	return ss->ss_family;
}
*/
import "C"

import (
	"io"
	"net/netip"
	"sync"
	"syscall"
	"time"
	"unsafe"

	"github.com/wzshiming/rdma/verbs"
)

// A PollFD converts to a struct pollfd by cast: the Events constants carry
// the poll(2) values, which this constant checks at compile time.
const _ = -uint((PollIn ^ C.POLLIN) | (PollOut ^ C.POLLOUT) | (PollErr ^ C.POLLERR) | (PollHup ^ C.POLLHUP) | (PollNval ^ C.POLLNVAL))

// Socket is a stream rsocket (rsocket(7)): a connected byte stream carried
// over an RDMA queue pair by librdmacm.
type Socket struct {
	fd int

	mu     sync.RWMutex // held shared by every call into rsockets, exclusively by Close
	closed bool
}

// New creates a stream socket of the address family domain (rsocket with
// SOCK_STREAM). It fails with a *verbs.OpError carrying the errno of
// librdmacm on a host without an RDMA device or the rdma_cm module.
func New(domain Domain) (*Socket, error) {
	var family C.int
	switch domain {
	case IPv4:
		family = C.AF_INET
	case IPv6:
		family = C.AF_INET6
	default:
		return nil, &verbs.OpError{Op: "rsocket", Err: syscall.EAFNOSUPPORT}
	}
	fd, err := C.rsocket(family, C.SOCK_STREAM, 0)
	if fd < 0 {
		return nil, opError("rsocket", err)
	}
	return &Socket{fd: int(fd)}, nil
}

// do runs f with the descriptor under the shared lock, or reports op called
// on a closed Socket.
func (s *Socket) do(op string, f func(fd C.int) error) error {
	s.mu.RLock()
	defer s.mu.RUnlock()
	if s.closed {
		return errClosed(op)
	}
	return f(C.int(s.fd))
}

// Descriptor returns the rsockets descriptor for Poll and diagnostics, or -1
// once the Socket is closed. It is not a kernel file descriptor: only the
// functions of this package may use it.
func (s *Socket) Descriptor() int {
	s.mu.RLock()
	defer s.mu.RUnlock()
	if s.closed {
		return -1
	}
	return s.fd
}

// sockaddr stores ap in ss and returns its length; a v4-mapped address
// becomes an IPv4 sockaddr.
func sockaddr(ss *C.struct_sockaddr_storage, ap netip.AddrPort) C.socklen_t {
	addr := ap.Addr().Unmap()
	if addr.Is4() {
		a := addr.As4()
		return C.rs_sockaddr(ss, C.AF_INET, (*C.uchar)(&a[0]), C.uint16_t(ap.Port()))
	}
	a := addr.As16()
	return C.rs_sockaddr(ss, C.AF_INET6, (*C.uchar)(&a[0]), C.uint16_t(ap.Port()))
}

// addrPort reads an IPv4 or IPv6 sockaddr; other families give the zero
// value.
func addrPort(ss *C.struct_sockaddr_storage) netip.AddrPort {
	var addr [16]byte
	var port C.uint16_t
	switch C.rs_addrport(ss, (*C.uchar)(&addr[0]), &port) {
	case C.AF_INET:
		return netip.AddrPortFrom(netip.AddrFrom4([4]byte(addr[:4])), uint16(port))
	case C.AF_INET6:
		return netip.AddrPortFrom(netip.AddrFrom16(addr), uint16(port))
	}
	return netip.AddrPort{}
}

func sockaddrPtr(ss *C.struct_sockaddr_storage) *C.struct_sockaddr {
	return (*C.struct_sockaddr)(unsafe.Pointer(ss))
}

// Bind binds the socket to local (rbind); its IP picks the RDMA device and
// port 0 picks a free port.
func (s *Socket) Bind(local netip.AddrPort) error {
	return s.do("rbind", func(fd C.int) error {
		var ss C.struct_sockaddr_storage
		n := sockaddr(&ss, local)
		if rc, err := C.rbind(fd, sockaddrPtr(&ss), n); rc != 0 {
			return opError("rbind", err)
		}
		return nil
	})
}

// Listen starts listening for connections (rlisten); backlog bounds the
// requests waiting to be accepted.
func (s *Socket) Listen(backlog int) error {
	return s.do("rlisten", func(fd C.int) error {
		if rc, err := C.rlisten(fd, C.int(backlog)); rc != 0 {
			return opError("rlisten", err)
		}
		return nil
	})
}

// Accept takes the next connection (raccept) and returns it with the peer's
// address. In non-blocking mode it fails with syscall.EAGAIN when no request
// is pending. The accepted Socket inherits the buffer, queue and inline
// options of the listener but not its non-blocking flag: call SetNonblock.
func (s *Socket) Accept() (*Socket, netip.AddrPort, error) {
	var conn *Socket
	var peer netip.AddrPort
	err := s.do("raccept", func(fd C.int) error {
		var ss C.struct_sockaddr_storage
		n := C.socklen_t(unsafe.Sizeof(ss))
		cfd, err := C.raccept(fd, sockaddrPtr(&ss), &n)
		if cfd < 0 {
			return opError("raccept", err)
		}
		conn = &Socket{fd: int(cfd)}
		peer = addrPort(&ss)
		return nil
	})
	if err != nil {
		return nil, netip.AddrPort{}, err
	}
	return conn, peer, nil
}

// Connect connects to remote (rconnect). In non-blocking mode it fails with
// syscall.EINPROGRESS at once and the attempt goes on as Poll, Read and
// Write are called: Poll reports PollOut when it ends, with PollErr when it
// failed, and SocketError tells how.
func (s *Socket) Connect(remote netip.AddrPort) error {
	return s.do("rconnect", func(fd C.int) error {
		var ss C.struct_sockaddr_storage
		n := sockaddr(&ss, remote)
		if rc, err := C.rconnect(fd, sockaddrPtr(&ss), n); rc != 0 {
			return opError("rconnect", err)
		}
		return nil
	})
}

// SocketError reads and clears SO_ERROR, the result of an asynchronous
// Connect: nil for success, otherwise a *verbs.OpError with Op "rconnect" and
// the errno.
func (s *Socket) SocketError() error {
	var value C.int
	err := s.do("rgetsockopt", func(fd C.int) error {
		if rc, err := C.rs_getsockopt_int(fd, C.SOL_SOCKET, C.SO_ERROR, &value); rc != 0 {
			return opError("rgetsockopt", err)
		}
		return nil
	})
	if err != nil {
		return err
	}
	if value == 0 {
		return nil
	}
	return &verbs.OpError{Op: "rconnect", Err: syscall.Errno(value)}
}

// Read reads from the connection (rrecv). It returns 0, io.EOF once the peer
// has shut down writing or closed and the buffered data is read, and in
// non-blocking mode an error matching syscall.EAGAIN when no data is ready.
func (s *Socket) Read(b []byte) (int, error) {
	if len(b) == 0 {
		return 0, nil
	}
	var n C.ssize_t
	err := s.do("rrecv", func(fd C.int) error {
		var err error
		n, err = C.rrecv(fd, unsafe.Pointer(&b[0]), C.size_t(len(b)), 0)
		if n < 0 {
			return opError("rrecv", err)
		}
		return nil
	})
	if err != nil {
		return 0, err
	}
	if n == 0 {
		return 0, io.EOF
	}
	return int(n), nil
}

// Write writes to the connection (rsend). It may write less than len(b) with
// a nil error, and in non-blocking mode returns 0 and an error matching
// syscall.EAGAIN when no buffer space is free.
func (s *Socket) Write(b []byte) (int, error) {
	if len(b) == 0 {
		return 0, nil
	}
	var n C.ssize_t
	err := s.do("rsend", func(fd C.int) error {
		var err error
		n, err = C.rsend(fd, unsafe.Pointer(&b[0]), C.size_t(len(b)), 0)
		if n < 0 {
			return opError("rsend", err)
		}
		return nil
	})
	if err != nil {
		return 0, err
	}
	return int(n), nil
}

// Shutdown closes one or both halves of the connection (rshutdown); the peer
// reads EOF after ShutWrite. It waits for the posted sends to complete.
func (s *Socket) Shutdown(how How) error {
	var chow C.int
	switch how {
	case ShutRead:
		chow = C.SHUT_RD
	case ShutWrite:
		chow = C.SHUT_WR
	case ShutReadWrite:
		chow = C.SHUT_RDWR
	default:
		return &verbs.OpError{Op: "rshutdown", Err: syscall.EINVAL}
	}
	return s.do("rshutdown", func(fd C.int) error {
		if rc, err := C.rshutdown(fd, chow); rc != 0 {
			return opError("rshutdown", err)
		}
		return nil
	})
}

// Close releases the socket (rclose) after waiting for the calls in progress
// on it and for its posted sends to complete. Every later call, Close
// included, fails with a *verbs.OpError wrapping os.ErrClosed, and the
// descriptor is never handed to librdmacm again.
func (s *Socket) Close() error {
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.closed {
		return errClosed("rclose")
	}
	s.closed = true
	if rc, err := C.rclose(C.int(s.fd)); rc != 0 {
		return opError("rclose", err)
	}
	return nil
}

// LocalAddr returns the local address of the socket (rgetsockname).
func (s *Socket) LocalAddr() (netip.AddrPort, error) {
	return s.addr("rgetsockname", func(fd C.int, sa *C.struct_sockaddr, n *C.socklen_t) (C.int, error) {
		rc, err := C.rgetsockname(fd, sa, n)
		return rc, err
	})
}

// RemoteAddr returns the address of the peer (rgetpeername).
func (s *Socket) RemoteAddr() (netip.AddrPort, error) {
	return s.addr("rgetpeername", func(fd C.int, sa *C.struct_sockaddr, n *C.socklen_t) (C.int, error) {
		rc, err := C.rgetpeername(fd, sa, n)
		return rc, err
	})
}

// addr reads an address of the socket with get, an rgetsockname or
// rgetpeername named op.
func (s *Socket) addr(op string, get func(C.int, *C.struct_sockaddr, *C.socklen_t) (C.int, error)) (netip.AddrPort, error) {
	var ap netip.AddrPort
	err := s.do(op, func(fd C.int) error {
		var ss C.struct_sockaddr_storage
		n := C.socklen_t(unsafe.Sizeof(ss))
		if rc, err := get(fd, sockaddrPtr(&ss), &n); rc != 0 {
			return opError(op, err)
		}
		ap = addrPort(&ss)
		return nil
	})
	return ap, err
}

// setsockopt sets an int option (rsetsockopt).
func (s *Socket) setsockopt(level, name C.int, value int) error {
	return s.do("rsetsockopt", func(fd C.int) error {
		if rc, err := C.rs_setsockopt_int(fd, level, name, C.int(value)); rc != 0 {
			return opError("rsetsockopt", err)
		}
		return nil
	})
}

func boolInt(on bool) int {
	if on {
		return 1
	}
	return 0
}

// SetSendBuffer sets the send buffer size (SO_SNDBUF); rsockets doubles it.
func (s *Socket) SetSendBuffer(bytes int) error {
	return s.setsockopt(C.SOL_SOCKET, C.SO_SNDBUF, bytes)
}

// SetReceiveBuffer sets the receive buffer size (SO_RCVBUF); rsockets
// doubles it.
func (s *Socket) SetReceiveBuffer(bytes int) error {
	return s.setsockopt(C.SOL_SOCKET, C.SO_RCVBUF, bytes)
}

// SetSendQueueSize sets the send queue depth (RDMA_SQSIZE).
func (s *Socket) SetSendQueueSize(n int) error { return s.setsockopt(C.SOL_RDMA, C.RDMA_SQSIZE, n) }

// SetReceiveQueueSize sets the receive queue depth (RDMA_RQSIZE).
func (s *Socket) SetReceiveQueueSize(n int) error {
	return s.setsockopt(C.SOL_RDMA, C.RDMA_RQSIZE, n)
}

// SetInline sets the largest send posted inline (RDMA_INLINE).
func (s *Socket) SetInline(bytes int) error { return s.setsockopt(C.SOL_RDMA, C.RDMA_INLINE, bytes) }

// SetReuseAddr sets SO_REUSEADDR; call it before Bind.
func (s *Socket) SetReuseAddr(on bool) error {
	return s.setsockopt(C.SOL_SOCKET, C.SO_REUSEADDR, boolInt(on))
}

// SetV6Only sets IPV6_V6ONLY; call it before Listen.
func (s *Socket) SetV6Only(on bool) error {
	return s.setsockopt(C.IPPROTO_IPV6, C.IPV6_V6ONLY, boolInt(on))
}

// SetNonblock sets or clears O_NONBLOCK (rfcntl F_GETFL/F_SETFL).
func (s *Socket) SetNonblock(on bool) error {
	return s.do("rfcntl", func(fd C.int) error {
		flags, err := C.rs_getfl(fd)
		if flags < 0 {
			return opError("rfcntl", err)
		}
		if on {
			flags |= C.O_NONBLOCK
		} else {
			flags &^= C.O_NONBLOCK
		}
		if rc, err := C.rs_setfl(fd, flags); rc != 0 {
			return opError("rfcntl", err)
		}
		return nil
	})
}

// Poll waits until an event of fds occurs or timeout passes (rpoll), fills
// in the Revents and returns the number of descriptors with events, 0 on
// timeout. A negative timeout waits without limit and 0 returns at once; an
// EINTR is retried for the time left. fds mixes rsockets descriptors and
// plain OS file descriptors freely and must not be empty.
func Poll(fds []PollFD, timeout time.Duration) (int, error) {
	if len(fds) == 0 {
		return 0, &verbs.OpError{Op: "rpoll", Err: syscall.EINVAL}
	}
	cfds := make([]C.struct_pollfd, len(fds))
	for i, fd := range fds {
		cfds[i] = C.struct_pollfd{fd: C.int(fd.Fd), events: C.short(fd.Events)}
	}
	var deadline time.Time
	if timeout > 0 {
		deadline = time.Now().Add(timeout)
	}
	for {
		n, err := C.rpoll(&cfds[0], C.nfds_t(len(cfds)), pollTimeout(timeout))
		if n >= 0 {
			for i := range fds {
				fds[i].Revents = Events(cfds[i].revents)
			}
			return int(n), nil
		}
		if err != syscall.EINTR {
			return 0, opError("rpoll", err)
		}
		// Past the deadline, one last immediate check fills in the Revents.
		if timeout > 0 {
			timeout = max(time.Until(deadline), 0)
		}
	}
}

// pollTimeout converts d to the milliseconds of rpoll: -1 for no limit, and
// rounded up so that a short wait does not become an immediate return.
func pollTimeout(d time.Duration) C.int {
	if d < 0 {
		return -1
	}
	ms := (d + time.Millisecond - 1) / time.Millisecond
	return C.int(min(ms, 1<<31-1))
}
