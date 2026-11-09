//go:build plan9

package rdma

import "syscall"

// Errors reported with the errno a socket would produce; Plan 9 has no
// ECONNRESET or EPIPE.
var (
	errConnReset error = syscall.NewError("connection reset by peer")
	errPipe      error = syscall.NewError("broken pipe")
)
