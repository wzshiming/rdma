// Package rsocket is a Go binding of rsockets, the socket API of librdmacm
// that carries a byte stream over an RDMA queue pair (rsocket(7)).
//
// A Socket follows the shape of a BSD stream socket: New creates it, Bind,
// Listen and Accept take the passive side, Connect the active one, Read and
// Write move data, Shutdown and Close end the connection, LocalAddr and
// RemoteAddr report the addresses, and the Set methods configure it. Poll
// wraps rpoll. Addresses are netip.AddrPort values; the local IP of Bind
// selects the RDMA device, as with rdma_cm.
//
// # Descriptors
//
// Socket.Descriptor returns the rsockets descriptor, which is the number of
// the kernel file descriptor of the socket's event channel: it never
// collides with a plain descriptor, so Poll takes both kinds in one call,
// but it must never be given to close, read, poll, epoll or any other system
// call. Once a Socket is closed its number may be reused by any open, so a
// descriptor must not outlive its Socket in a PollFD.
//
// # Non-blocking mode
//
// After SetNonblock(true), Accept fails with syscall.EAGAIN when no request
// is pending, Read with syscall.EAGAIN when no data is ready, Write writes
// what fits and fails with syscall.EAGAIN when nothing does, and Connect
// fails with syscall.EINPROGRESS at once. The attempt then goes on as Poll,
// Read or Write are called on the socket: Poll reports PollOut once it has
// ended, together with PollErr when it failed, and SocketError returns the
// errno — syscall.ECONNREFUSED for a port nobody listens on. Accepted
// sockets do not inherit the flag: an Accept from a non-blocking listener
// returns a socket whose Read blocks until SetNonblock(true) is called on it.
//
// Flow control runs on credits: a Write fills the sender's buffer and the
// receiver's, 128 KiB each by default, and then fails with syscall.EAGAIN
// until the receiver has returned the space. Read alone does not return it:
// the receiver sends the credit update from its next call that services the
// socket, a Poll on it, a Read that has to wait, or a Write. An event loop
// that polls every connected socket does this as a matter of course.
//
// Poll on an rsockets descriptor first spins for the polling_time of the
// configuration (10 µs by default), then sleeps in poll(2) in slices of
// wake_up_interval (5 s by default), so a call without a timeout wakes
// briefly now and then; an interrupted sleep is retried for the time left.
// Plain descriptors are checked in both phases. A positive timeout runs on
// a timerfd polled along with the descriptors rather than on rpoll's own
// timeout: in rdma-core up to v60 an rpoll whose timeout expires leaks its
// poller count, after which every rpoll that has to wait spins instead of
// sleeping and never times out (fixed upstream after v60 by "librdmacm: Fix
// rpoll in case of timeout").
//
// # Options
//
// The Set methods take effect before Connect or Listen: SetSendBuffer and
// SetReceiveBuffer size the registered send and receive buffers, which
// rsockets doubles like the kernel does, SetSendQueueSize and
// SetReceiveQueueSize the work queues, SetInline the largest send posted
// inline. Sockets returned by Accept inherit these values from the
// listener. The defaults, 128 KiB buffers, 384 entries and 64 bytes inline,
// come from the process-wide configuration files
// /etc/rdma/rsocket/{mem_default,wmem_default,sqsize_default,rqsize_default,
// inline_default,iomap_size,polling_time,wake_up_interval}, read once when
// the first socket is created. SetReuseAddr must precede Bind and
// SetV6Only Listen.
//
// # Blocking and concurrency
//
// Every call runs inside librdmacm on the calling thread, and librdmacm
// runs threads of its own for the connection management: a listener's
// requests are accepted there, handshake included, before Poll reports it
// readable, and a non-blocking Connect goes on there even while the socket
// is not polled. Shutdown and Close wait for the posted sends to complete,
// which may take the retry timeout of the transport, several seconds, when
// the peer has vanished. On a non-blocking socket one goroutine may Read,
// another Write and a third Poll at the same time; a blocking socket must
// not be shared, since its completion channel blocks the waiter inside C.
// Close waits for the calls in progress on the Socket to return before
// releasing it, so a blocking call that never returns holds Close back: use
// non-blocking mode and Poll, with a pipe or eventfd among the descriptors
// for wake-ups. After Close every method fails with an error wrapping
// os.ErrClosed and the descriptor is never passed to librdmacm again.
//
// Registered memory does not survive fork: a process that forks after
// creating a socket needs RDMAV_FORK_SAFE=1 in its environment, or
// ibv_fork_init before the first socket.
//
// # Errors
//
// Errors from librdmacm are *verbs.OpError values whose Op names the
// rsockets function ("rsocket", "rbind", "rlisten", "raccept", "rconnect",
// "rrecv", "rsend", "rshutdown", "rclose", "rgetsockname", "rgetpeername",
// "rsetsockopt", "rgetsockopt", "rfcntl", "rpoll") and whose Err is the
// syscall.Errno, or os.ErrClosed after Close.
//
// On Linux with cgo the package links against librdmacm (-lrdmacm, header
// rdma/rsocket.h). Everywhere else it compiles to stubs whose New and Poll
// return an error satisfying errors.Is(err, errors.ErrUnsupported); the
// types, constants and String methods are available on every platform.
package rsocket
