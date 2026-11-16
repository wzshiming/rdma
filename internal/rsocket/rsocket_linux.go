//go:build linux && !android && (amd64 || arm64 || loong64 || ppc64le || riscv64 || s390x)

package rsocket

import (
	"encoding/binary"
	"io"
	"net/netip"
	"sync"
	"sync/atomic"
	"syscall"
	"time"
	"unsafe"

	"github.com/wzshiming/rdma/internal/dynlib"
	"github.com/wzshiming/rdma/verbs"
)

// rdmacm is the librdmacm of the process, loaded on first use; a test points
// it at a library that does not exist.
var rdmacm = &loader{name: "librdmacm.so.1"}

// loader resolves the rsockets functions of the library name once, on first
// use. A load that fails is reported to its caller and tried again by the
// next one.
type loader struct {
	name string
	mu   sync.Mutex
	lib  atomic.Pointer[library]
}

// library is the rsockets functions of librdmacm.
type library struct {
	rsocket, rbind, rlisten, raccept, rconnect, rrecv, rsend, rshutdown, rclose,
	rgetsockname, rgetpeername, rpoll, rsetsockopt, rgetsockopt, rfcntl dynlib.Func
}

// load returns the functions of the library, loading it on the first call.
// A library the dynamic loader cannot open, or one lacking a function, is a
// *verbs.OpError of op that matches errors.ErrUnsupported and names the
// library.
func (l *loader) load(op string) (*library, error) {
	if lib := l.lib.Load(); lib != nil {
		return lib, nil
	}
	l.mu.Lock()
	defer l.mu.Unlock()
	if lib := l.lib.Load(); lib != nil {
		return lib, nil
	}
	so, err := dynlib.Open(l.name)
	if err != nil {
		return nil, &verbs.OpError{Op: op, Err: err}
	}
	lib := new(library)
	for _, f := range []struct {
		name string
		fn   *dynlib.Func
	}{
		{"rsocket", &lib.rsocket}, {"rbind", &lib.rbind}, {"rlisten", &lib.rlisten},
		{"raccept", &lib.raccept}, {"rconnect", &lib.rconnect}, {"rrecv", &lib.rrecv},
		{"rsend", &lib.rsend}, {"rshutdown", &lib.rshutdown}, {"rclose", &lib.rclose},
		{"rgetsockname", &lib.rgetsockname}, {"rgetpeername", &lib.rgetpeername},
		{"rpoll", &lib.rpoll}, {"rsetsockopt", &lib.rsetsockopt},
		{"rgetsockopt", &lib.rgetsockopt}, {"rfcntl", &lib.rfcntl},
	} {
		if *f.fn, err = so.Lookup(f.name); err != nil {
			return nil, &verbs.OpError{Op: op, Err: err}
		}
	}
	l.lib.Store(lib)
	return lib, nil
}

// Options of the level SOL_RDMA, from rdma/rsocket.h.
const (
	solRDMA    = 0x10000
	rdmaSQSize = 0 // RDMA_SQSIZE
	rdmaRQSize = 1 // RDMA_RQSIZE
	rdmaInline = 2 // RDMA_INLINE
)

// Socket is a stream rsocket (rsocket(7)): a connected byte stream carried
// over an RDMA queue pair by librdmacm.
type Socket struct {
	lib *library
	fd  int

	mu     sync.RWMutex // held shared by every call into rsockets, exclusively by Close
	closed bool
}

// New creates a stream socket of the address family domain (rsocket with
// SOCK_STREAM). It fails with a *verbs.OpError carrying the errno of
// librdmacm on a host without an RDMA device or the rdma_cm module, and
// with one matching errors.ErrUnsupported when librdmacm.so.1 cannot be
// loaded.
func New(domain Domain) (*Socket, error) {
	var family uintptr
	switch domain {
	case IPv4:
		family = syscall.AF_INET
	case IPv6:
		family = syscall.AF_INET6
	default:
		return nil, &verbs.OpError{Op: "rsocket", Err: syscall.EAFNOSUPPORT}
	}
	lib, err := rdmacm.load("rsocket")
	if err != nil {
		return nil, err
	}
	fd, errno := lib.rsocket.Int(family, syscall.SOCK_STREAM, 0, 0, 0, 0)
	if fd < 0 {
		return nil, opError("rsocket", errno)
	}
	return &Socket{lib: lib, fd: int(fd)}, nil
}

// do runs f with the descriptor under the shared lock, or reports op called
// on a closed Socket.
func (s *Socket) do(op string, f func(fd uintptr) error) error {
	s.mu.RLock()
	defer s.mu.RUnlock()
	if s.closed {
		return errClosed(op)
	}
	return f(uintptr(s.fd))
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

// sockaddrIn and sockaddrIn6 are struct sockaddr_in and struct sockaddr_in6:
// the family an sa_family_t in the host's byte order, the port two bytes in
// network order whatever the host's, then the address; sin6_flowinfo and
// sin6_scope_id are neither set nor read.
type sockaddrIn struct {
	family uint16
	port   [2]byte
	addr   [4]byte
	_      [8]byte
}

type sockaddrIn6 struct {
	family   uint16
	port     [2]byte
	flowinfo uint32
	addr     [16]byte
	scopeID  uint32
}

// sockaddrStorage is struct sockaddr_storage, which holds either of them and
// is aligned like it.
type sockaddrStorage struct {
	family uint16
	_      [6]byte
	_      [15]uint64
}

// set stores ap in ss and returns the length of the sockaddr (a socklen_t);
// a v4-mapped address becomes an IPv4 sockaddr.
func (ss *sockaddrStorage) set(ap netip.AddrPort) uint32 {
	*ss = sockaddrStorage{}
	var port [2]byte
	binary.BigEndian.PutUint16(port[:], ap.Port())
	addr := ap.Addr().Unmap()
	if addr.Is4() {
		sin := (*sockaddrIn)(unsafe.Pointer(ss))
		*sin = sockaddrIn{family: syscall.AF_INET, port: port, addr: addr.As4()}
		return uint32(unsafe.Sizeof(*sin))
	}
	sin6 := (*sockaddrIn6)(unsafe.Pointer(ss))
	*sin6 = sockaddrIn6{family: syscall.AF_INET6, port: port, addr: addr.As16()}
	return uint32(unsafe.Sizeof(*sin6))
}

// addrPort reads an IPv4 or IPv6 sockaddr; other families give the zero
// value.
func (ss *sockaddrStorage) addrPort() netip.AddrPort {
	switch ss.family {
	case syscall.AF_INET:
		sin := (*sockaddrIn)(unsafe.Pointer(ss))
		return netip.AddrPortFrom(netip.AddrFrom4(sin.addr), binary.BigEndian.Uint16(sin.port[:]))
	case syscall.AF_INET6:
		sin6 := (*sockaddrIn6)(unsafe.Pointer(ss))
		return netip.AddrPortFrom(netip.AddrFrom16(sin6.addr), binary.BigEndian.Uint16(sin6.port[:]))
	}
	return netip.AddrPort{}
}

// Bind binds the socket to local (rbind); its IP picks the RDMA device and
// port 0 picks a free port.
func (s *Socket) Bind(local netip.AddrPort) error {
	return s.do("rbind", func(fd uintptr) error {
		var ss sockaddrStorage
		n := ss.set(local)
		if rc, errno := s.lib.rbind.Int(fd, uintptr(unsafe.Pointer(&ss)), uintptr(n), 0, 0, 0); rc != 0 {
			return opError("rbind", errno)
		}
		return nil
	})
}

// Listen starts listening for connections (rlisten); backlog bounds the
// requests waiting to be accepted.
func (s *Socket) Listen(backlog int) error {
	return s.do("rlisten", func(fd uintptr) error {
		if rc, errno := s.lib.rlisten.Int(fd, uintptr(backlog), 0, 0, 0, 0); rc != 0 {
			return opError("rlisten", errno)
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
	err := s.do("raccept", func(fd uintptr) error {
		var ss sockaddrStorage
		n := uint32(unsafe.Sizeof(ss))
		cfd, errno := s.lib.raccept.Int(fd, uintptr(unsafe.Pointer(&ss)), uintptr(unsafe.Pointer(&n)), 0, 0, 0)
		if cfd < 0 {
			return opError("raccept", errno)
		}
		conn = &Socket{lib: s.lib, fd: int(cfd)}
		peer = ss.addrPort()
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
	return s.do("rconnect", func(fd uintptr) error {
		var ss sockaddrStorage
		n := ss.set(remote)
		if rc, errno := s.lib.rconnect.Int(fd, uintptr(unsafe.Pointer(&ss)), uintptr(n), 0, 0, 0); rc != 0 {
			return opError("rconnect", errno)
		}
		return nil
	})
}

// SocketError reads and clears SO_ERROR, the result of an asynchronous
// Connect: nil for success, otherwise a *verbs.OpError with Op "rconnect" and
// the errno.
func (s *Socket) SocketError() error {
	var value int32
	err := s.do("rgetsockopt", func(fd uintptr) error {
		n := uint32(unsafe.Sizeof(value))
		if rc, errno := s.lib.rgetsockopt.Int(fd, syscall.SOL_SOCKET, syscall.SO_ERROR, uintptr(unsafe.Pointer(&value)), uintptr(unsafe.Pointer(&n)), 0); rc != 0 {
			return opError("rgetsockopt", errno)
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
	var n int
	err := s.do("rrecv", func(fd uintptr) error {
		var errno syscall.Errno
		n, errno = s.lib.rrecv.SSize(fd, uintptr(unsafe.Pointer(&b[0])), uintptr(len(b)), 0, 0, 0)
		if n < 0 {
			return opError("rrecv", errno)
		}
		return nil
	})
	if err != nil {
		return 0, err
	}
	if n == 0 {
		return 0, io.EOF
	}
	return n, nil
}

// Write writes to the connection (rsend). It may write less than len(b) with
// a nil error, and in non-blocking mode returns 0 and an error matching
// syscall.EAGAIN when no buffer space is free.
func (s *Socket) Write(b []byte) (int, error) {
	if len(b) == 0 {
		return 0, nil
	}
	var n int
	err := s.do("rsend", func(fd uintptr) error {
		var errno syscall.Errno
		n, errno = s.lib.rsend.SSize(fd, uintptr(unsafe.Pointer(&b[0])), uintptr(len(b)), 0, 0, 0)
		if n < 0 {
			return opError("rsend", errno)
		}
		return nil
	})
	if err != nil {
		return 0, err
	}
	return n, nil
}

// Shutdown closes one or both halves of the connection (rshutdown); the peer
// reads EOF after ShutWrite. It waits for the posted sends to complete.
func (s *Socket) Shutdown(how How) error {
	var chow uintptr
	switch how {
	case ShutRead:
		chow = syscall.SHUT_RD
	case ShutWrite:
		chow = syscall.SHUT_WR
	case ShutReadWrite:
		chow = syscall.SHUT_RDWR
	default:
		return &verbs.OpError{Op: "rshutdown", Err: syscall.EINVAL}
	}
	return s.do("rshutdown", func(fd uintptr) error {
		if rc, errno := s.lib.rshutdown.Int(fd, chow, 0, 0, 0, 0); rc != 0 {
			return opError("rshutdown", errno)
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
	if rc, errno := s.lib.rclose.Int(uintptr(s.fd), 0, 0, 0, 0, 0); rc != 0 {
		return opError("rclose", errno)
	}
	return nil
}

// LocalAddr returns the local address of the socket (rgetsockname).
func (s *Socket) LocalAddr() (netip.AddrPort, error) {
	return s.addr("rgetsockname", s.lib.rgetsockname)
}

// RemoteAddr returns the address of the peer (rgetpeername).
func (s *Socket) RemoteAddr() (netip.AddrPort, error) {
	return s.addr("rgetpeername", s.lib.rgetpeername)
}

// addr reads an address of the socket with get, rgetsockname or rgetpeername
// named op.
func (s *Socket) addr(op string, get dynlib.Func) (netip.AddrPort, error) {
	var ap netip.AddrPort
	err := s.do(op, func(fd uintptr) error {
		var ss sockaddrStorage
		n := uint32(unsafe.Sizeof(ss))
		if rc, errno := get.Int(fd, uintptr(unsafe.Pointer(&ss)), uintptr(unsafe.Pointer(&n)), 0, 0, 0); rc != 0 {
			return opError(op, errno)
		}
		ap = ss.addrPort()
		return nil
	})
	return ap, err
}

// setsockopt sets an int option (rsetsockopt).
func (s *Socket) setsockopt(level, name int, value int) error {
	return s.do("rsetsockopt", func(fd uintptr) error {
		v := int32(value)
		if rc, errno := s.lib.rsetsockopt.Int(fd, uintptr(level), uintptr(name), uintptr(unsafe.Pointer(&v)), unsafe.Sizeof(v), 0); rc != 0 {
			return opError("rsetsockopt", errno)
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
	return s.setsockopt(syscall.SOL_SOCKET, syscall.SO_SNDBUF, bytes)
}

// SetReceiveBuffer sets the receive buffer size (SO_RCVBUF); rsockets
// doubles it.
func (s *Socket) SetReceiveBuffer(bytes int) error {
	return s.setsockopt(syscall.SOL_SOCKET, syscall.SO_RCVBUF, bytes)
}

// SetSendQueueSize sets the send queue depth (RDMA_SQSIZE).
func (s *Socket) SetSendQueueSize(n int) error { return s.setsockopt(solRDMA, rdmaSQSize, n) }

// SetReceiveQueueSize sets the receive queue depth (RDMA_RQSIZE).
func (s *Socket) SetReceiveQueueSize(n int) error { return s.setsockopt(solRDMA, rdmaRQSize, n) }

// SetInline sets the largest send posted inline (RDMA_INLINE).
func (s *Socket) SetInline(bytes int) error { return s.setsockopt(solRDMA, rdmaInline, bytes) }

// SetReuseAddr sets SO_REUSEADDR; call it before Bind.
func (s *Socket) SetReuseAddr(on bool) error {
	return s.setsockopt(syscall.SOL_SOCKET, syscall.SO_REUSEADDR, boolInt(on))
}

// SetV6Only sets IPV6_V6ONLY; call it before Listen.
func (s *Socket) SetV6Only(on bool) error {
	return s.setsockopt(syscall.IPPROTO_IPV6, syscall.IPV6_V6ONLY, boolInt(on))
}

// SetNonblock sets or clears O_NONBLOCK (rfcntl F_GETFL/F_SETFL).
func (s *Socket) SetNonblock(on bool) error {
	return s.do("rfcntl", func(fd uintptr) error {
		flags, errno := s.lib.rfcntl.Int(fd, syscall.F_GETFL, 0, 0, 0, 0)
		if flags < 0 {
			return opError("rfcntl", errno)
		}
		if on {
			flags |= syscall.O_NONBLOCK
		} else {
			flags &^= syscall.O_NONBLOCK
		}
		if rc, errno := s.lib.rfcntl.Int(fd, syscall.F_SETFL, uintptr(flags), 0, 0, 0); rc != 0 {
			return opError("rfcntl", errno)
		}
		return nil
	})
}

// pollfd is struct pollfd; the Events constants carry the values of poll(2),
// which are the same on every Linux architecture.
type pollfd struct {
	fd      int32
	events  int16
	revents int16
}

// clockMonotonic is CLOCK_MONOTONIC, the same on every Linux architecture.
const clockMonotonic = 1

// itimerspec is struct itimerspec.
type itimerspec struct {
	interval syscall.Timespec
	value    syscall.Timespec
}

// timerfd returns a descriptor that becomes readable once d has passed: a
// non-blocking timerfd on CLOCK_MONOTONIC (TFD_NONBLOCK and TFD_CLOEXEC are
// O_NONBLOCK and O_CLOEXEC).
func timerfd(d time.Duration) (int, error) {
	fd, _, errno := syscall.Syscall(syscall.SYS_TIMERFD_CREATE, clockMonotonic, syscall.O_NONBLOCK|syscall.O_CLOEXEC, 0)
	if errno != 0 {
		return -1, opError("timerfd_create", errno)
	}
	its := itimerspec{value: syscall.NsecToTimespec(int64(d))}
	if _, _, errno := syscall.Syscall6(syscall.SYS_TIMERFD_SETTIME, fd, 0, uintptr(unsafe.Pointer(&its)), 0, 0, 0); errno != 0 {
		syscall.Close(int(fd))
		return -1, opError("timerfd_settime", errno)
	}
	return int(fd), nil
}

// Poll waits until an event of fds occurs or timeout passes (rpoll), fills
// in the Revents and returns the number of descriptors with events, 0 on
// timeout. A negative timeout waits without limit and 0 returns at once; an
// EINTR is retried for the time left. fds mixes rsockets descriptors and
// plain OS file descriptors freely and must not be empty. It fails with an
// error matching errors.ErrUnsupported when librdmacm.so.1 cannot be loaded.
func Poll(fds []PollFD, timeout time.Duration) (int, error) {
	if len(fds) == 0 {
		return 0, &verbs.OpError{Op: "rpoll", Err: syscall.EINVAL}
	}
	lib, err := rdmacm.load("rpoll")
	if err != nil {
		return 0, err
	}
	cfds := make([]pollfd, len(fds), len(fds)+1)
	for i, fd := range fds {
		cfds[i] = pollfd{fd: int32(fd.Fd), events: int16(fd.Events)}
	}
	// The timeout runs on a timerfd polled along with fds while rpoll waits
	// without limit: in rdma-core up to v60 an rpoll whose own timeout
	// expires leaks its poller count, after which every rpoll that has to
	// wait spins instead of sleeping and never times out.
	wait := -1
	switch {
	case timeout == 0:
		wait = 0
	case timeout > 0:
		tfd, err := timerfd(timeout)
		if err != nil {
			return 0, err
		}
		defer syscall.Close(tfd)
		cfds = append(cfds, pollfd{fd: int32(tfd), events: int16(PollIn)})
	}
	for {
		n, errno := lib.rpoll.Int(uintptr(unsafe.Pointer(&cfds[0])), uintptr(len(cfds)), uintptr(wait), 0, 0, 0)
		if n < 0 {
			if errno == syscall.EINTR {
				continue
			}
			return 0, opError("rpoll", errno)
		}
		ready := 0
		for i := range fds {
			fds[i].Revents = Events(cfds[i].revents)
			if fds[i].Revents != 0 {
				ready++
			}
		}
		return ready, nil
	}
}
