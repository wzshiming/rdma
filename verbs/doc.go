// Package verbs is a Go binding of libibverbs, the user-space RDMA verbs
// library of rdma-core.
//
// It exposes devices, protection domains, memory regions, completion queues
// and channels, queue pairs, work requests, work completions and asynchronous
// events with Go types only; no C type appears in the exported API.
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
// Waiting for completion events (CompChannel.Wait) and asynchronous events
// (Context.WaitAsyncEvent) happens on the Go runtime poller: no OS thread is
// blocked inside C, cancellation and timeouts come from the context, and every
// event is acknowledged before Wait returns.
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
