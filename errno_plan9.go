//go:build plan9

package rdma

import "syscall"

// Errors of the socket layer, with the errno rsockets use; Plan 9 has no
// errnos.
var (
	errAgain      error = syscall.NewError("resource temporarily unavailable")
	errInProgress error = syscall.NewError("operation now in progress")
	errPipe       error = syscall.NewError("broken pipe")
	errConnReset  error = syscall.NewError("connection reset by peer")
)
