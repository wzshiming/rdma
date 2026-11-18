// Package verbs is a Go binding of libibverbs, the user-space RDMA verbs
// library of rdma-core.
//
// It exposes devices, protection domains, memory regions, completion queues
// and channels, queue pairs, shared receive queues, work requests, work
// completions and asynchronous events with Go types only; no C type appears
// in the exported API.
//
// On Linux (amd64, arm64, loong64, ppc64le, riscv64 and s390x) the package
// loads libibverbs.so.1 at run time on first use, with or without cgo, so
// building needs neither a C compiler nor the rdma-core headers; when the
// library cannot be loaded, every entry point returns an *OpError that
// satisfies errors.Is(err, errors.ErrUnsupported). Everywhere else it
// compiles to stubs whose constructors return the same kind of error; the
// types, constants and String methods are available on every platform.
//
// Every failing rdma-core call is reported as an *OpError whose Err is the
// syscall.Errno the call produced, so errors.Is(err, syscall.ENOMEM) and
// friends work. A failed work completion is reported by WC.Err as a *WCError.
//
// Immediate data (SendWR.Imm, WC.Imm) travels in network byte order, as the
// __be32 of the C API does: a C peer reads a value sent from Go with ntohl
// and sends one for Go with htonl.
//
// An unreliable-datagram (UD) queue pair addresses each send on its own: the
// SendWR names an address handle (PD.CreateAH), the receiving queue pair and
// its qkey. QP.AttachMulticast has it receive the datagrams of a multicast
// group as well. Every receive buffer of a UD queue pair begins with 40 bytes
// for the global routing header (GRH) of the datagram, which the device
// writes when the completion has the WCGRH flag (on RoCE v2 over IPv4 the
// last 20 of them hold the IPv4 header); the payload follows, and WC.Len
// counts both. The completion names the sender: WC.SrcQP is the number of its
// queue pair, WC.SLID and WC.SL its LID and service level on InfiniBand, and
// the source GID of the header, or on RoCE v2 over IPv4 the source address,
// its port; PD.CreateAHFromWC turns the completion and the header into the
// address handle that answers the sender.
//
// A shared receive queue (PD.CreateSRQ) is the receive queue of every queue
// pair created with it in QPInitAttr.SRQ: SRQ.PostRecv posts the receives,
// and each completes on the receive CQ of the queue pair the message came in
// on, which WC.QPNum names. SRQ.Modify arms its limit, after which
// Context.WaitAsyncEvent reports EventSRQLimitReached for it once fewer
// receives than the limit are left.
//
// XRC goes through an XRC domain (Context.OpenXRCD): an XRC send queue pair
// (QPTypeXRCSend) sends to an SRQ of type SRQTypeXRC in the domain of the
// remote side by the SRQ's number (SRQ.Num, SendWR.RemoteSRQN), through the
// XRC receive queue pair it is connected to (QPTypeXRCRecv, created with
// QPInitAttr.XRCD), and the receive completes on the SRQ's CQ. These verbs
// come from the extended ops of the device context, which a device or a
// library may lack: then they report syscall.EOPNOTSUPP, which matches
// errors.ErrUnsupported, as on rxe and siw, which have no XRC. Finding the
// ops assumes glibc's pthread_mutex_t, which precedes the member of struct
// ibv_context that marks it extended: with musl on arm64, whose mutex is 8
// bytes shorter, the lookup misses that member and these verbs report
// syscall.EOPNOTSUPP too.
//
// Waiting for completion events (CompChannel.Wait) and asynchronous events
// (Context.WaitAsyncEvent) happens on the Go runtime poller: no OS thread is
// blocked inside C, cancellation and timeouts come from the context, and the
// waits acknowledge the events they return: at once, but for the completion
// events of a CQ of CreateCQ, which are acknowledged in batches and by
// CQ.Close.
//
// Memory registered with PD.RegisterMemory is pinned with runtime.Pinner until
// MR.Close; PD.AllocMemory returns page-aligned memory outside the Go heap.
//
// Advanced use: the Pointer methods and the *FromPointer constructors exchange
// raw rdma-core handles with other packages built on rdma-core (such as a
// librdmacm binding). A wrapper made by a FromPointer constructor does not own
// the handle: its Close releases only Go-side resources.
//
// ForkInit wraps ibv_fork_init; the package never calls it implicitly, and
// kernels 5.12 and newer do not need it.
package verbs
