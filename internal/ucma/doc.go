// Package ucma drives the kernel's RDMA connection manager through its
// device, /dev/infiniband/rdma_cm, the interface librdmacm is built on. A
// Channel is one open descriptor of the device; each of its command methods
// writes one command of version 4 of the kernel's ABI (the structures of
// rdma/rdma_cma_abi.h), and GetEvent fetches the events the kernel queues on
// the channel. Nothing here needs libibverbs or librdmacm: the queue pairs
// the commands configure are the caller's.
//
// An identifier is the kernel's number of an rdma_cm_id, valid on the
// channel that holds it until MigrateID moves it to another. It carries the
// caller's uid, which its events echo; the kernel drops the events of an
// identifier whose uid is 0. Closing a channel destroys the identifiers it
// holds.
//
// Errors are syscall.Errno values or wrap one: the errno of the write that
// carried the command, ENODATA when the kernel took less than the whole
// command, as librdmacm reports it, EINVAL for an argument no command can
// carry, and for Open the errno of open(2), ENOENT when the kernel has no
// rdma_cm device. Calls on a closed Channel return os.ErrClosed, and a wait
// returns ctx.Err() when its context ends first.
//
// The package builds on Linux for the architectures of package cm; on other
// platforms it is empty.
package ucma
