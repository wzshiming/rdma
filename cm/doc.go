// Package cm is the RDMA connection manager of librdmacm, rdma-core's
// connection manager library, implemented in Go on the kernel's rdma_cm
// device.
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
// passed to ID.CreateQP must belong to ID.Device(), the device context the
// package opened for the connection, or be nil for the device's default one;
// that context and that protection domain stay open for the life of the
// process, so completion queues and protection domains may outlive the ID,
// while the queue pair is destroyed by ID.Close.
//
// Each ID owns an event channel, a descriptor of the kernel's device that
// waits on the Go runtime poller: no OS thread blocks, cancellation comes from
// the context, and Close never blocks, save on iWARP, where it waits in the
// kernel for a Connect under way to end.
//
// The IB CM carries private data in fixed-size fields: MaxConnectPrivateData
// bytes in a request, MaxAcceptPrivateData in a reply, MaxRejectPrivateData in
// a reject. The receiving side sees the whole field, zero padded, so callers
// frame their own data. Longer data is rejected before any call.
//
// Errors are *verbs.OpError values whose Op names the librdmacm function that
// does the same. A connection that fails is reported with the errno a socket
// would produce: syscall.ECONNREFUSED when nobody listens on the port,
// syscall.ETIMEDOUT or syscall.EHOSTUNREACH when the peer cannot be reached,
// syscall.ECONNRESET when it disappears. A peer that calls Request.Reject
// produces a *RejectedError carrying its private data, which also matches
// syscall.ECONNREFUSED in errors.Is. Using an ID after Close yields an error
// wrapping os.ErrClosed; Accept on a closed Listener yields net.ErrClosed.
//
// The package does what librdmacm does for the functions it offers, with the
// commands of the kernel's device /dev/infiniband/rdma_cm (ABI version 4) and
// with package verbs on the devices, and a peer may run either. That includes
// the vendor option exchange (ECE) of ID.CreateQP, ID.Connect and
// Request.Accept, which tests check step by step without a device and through
// the kernel against a peer on Soft-RoCE, whose queue pairs have no ECE unless
// a test simulates it; no device that supports ECE has run the
// exchange. It departs from librdmacm where nothing it offers depends on it:
// it asks no ibacm for the routes of InfiniBand and reads none of the GID and
// path records of a connection; and it keeps the default protection domain of
// a device for the life of the process, where librdmacm frees it with the
// device's last identifier.
//
// It also departs in two lifetimes. The queue pair belongs to its ID, and
// the *verbs.QP that ID.CreateQP and ID.QP return releases nothing on Close,
// but the QP of a verbs.AsyncEvent about it is the owning verbs object, whose
// Close destroys the queue pair under the ID, where with librdmacm it
// releases nothing. And an ID or Listener that becomes unreachable without
// Close is torn down when the garbage collector finalizes the descriptor of
// its event channel, where librdmacm keeps it until the process exits; Close
// stays required: the collector picks the moment, and only Close destroys
// the queue pair.
//
// On iWARP, whose connection manager in the kernel connects queue pairs over
// TCP, the package takes librdmacm's iWARP paths, which CI verifies on
// soft-iWARP (siw) with the device tests it runs on Soft-RoCE: the kernel,
// not the package, takes a queue pair to RTS, and Disconnect moves it to SQD
// where it moves it to ERR elsewhere. MPA carries private data with its
// length, so the peer receives what was sent, unpadded; an MPA reject carries
// no reason, so the Reason of a *RejectedError is the kernel's negative errno,
// and a reject without private data fails Connect as a refused connection
// does; and Close of an ID whose Connect is under way waits until the kernel
// ends the attempt, for up to 10 seconds on siw. Hardware iWARP devices and
// the iWARP port mapper, which does not run in CI, are not verified.
//
// On Linux (amd64, arm64, loong64, ppc64le, riscv64 and s390x) the package
// needs the kernel's rdma_cm device and libibverbs with a provider for the
// device, which package verbs loads at run time on first use, with or without
// cgo, so building needs neither a C compiler nor the rdma-core headers; it
// does not use librdmacm. When libibverbs cannot be loaded, Resolve and
// Listen return a *verbs.OpError that satisfies errors.Is(err,
// errors.ErrUnsupported); on a host without a device they fail with
// syscall.ENODEV. Everywhere else the package compiles to stubs whose
// constructors return a *verbs.OpError matching errors.ErrUnsupported; the
// types, constants and String methods are available on every platform.
package cm
