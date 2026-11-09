// Package cm is a Go binding of librdmacm, the RDMA connection manager
// library of rdma-core.
//
// It establishes and tears down reliable connected (RC) connections with the
// vocabulary of the net package: Resolve turns a pair of IP addresses into an
// ID bound to a device and port, ID.CreateQP creates the queue pair of the
// connection, ID.Connect connects it, Listen and Listener.Accept take the
// passive side, Request.Accept or Request.Reject answer a request, and
// ID.Disconnect, ID.Wait and ID.Close run the rest of the connection's life.
// Every wait takes a context and returns ctx.Err() when it ends first.
//
// Addresses are IP addresses with ports (netip.AddrPort); host names are the
// caller's business. rdma_cm picks the device by the local IP address, so a
// host with several RDMA devices chooses one through the local address of
// Resolve or Listen.
//
// Queue pairs and protection domains are verbs types. The protection domain
// passed to ID.CreateQP must belong to ID.Device(), the device context rdma_cm
// opened for the connection; that context stays open for the life of the
// process, so completion queues and protection domains may outlive the ID,
// while the queue pair is destroyed by ID.Close.
//
// Each ID owns an event channel whose descriptor waits on the Go runtime
// poller: no OS thread blocks inside C, and cancellation comes from the
// context. Every event is acknowledged as soon as it is read, which keeps
// rdma_destroy_id from waiting, so Close never blocks.
//
// The IB CM carries private data in fixed-size fields: MaxConnectPrivateData
// bytes in a request, MaxAcceptPrivateData in a reply, MaxRejectPrivateData in
// a reject. The receiving side sees the whole field, zero padded, so callers
// frame their own data. Longer data is rejected before any call.
//
// Errors from rdma-core are *verbs.OpError values whose Op names the librdmacm
// function. A connection that fails is reported with the errno a socket would
// produce: syscall.ECONNREFUSED when nobody listens on the port,
// syscall.ETIMEDOUT or syscall.EHOSTUNREACH when the peer cannot be reached,
// syscall.ECONNRESET when it disappears. A peer that calls Request.Reject
// produces a *RejectedError carrying its private data, which also matches
// syscall.ECONNREFUSED in errors.Is. Using an ID after Close yields an error
// wrapping os.ErrClosed; Accept on a closed Listener yields net.ErrClosed.
//
// On Linux with cgo the package links against librdmacm (-lrdmacm, header
// rdma/rdma_cma.h). Everywhere else it compiles to stubs whose constructors
// return an error satisfying errors.Is(err, errors.ErrUnsupported); the
// types, constants and String methods are available on every platform.
package cm
