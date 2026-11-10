//go:build plan9

package rdma

import "syscall"

// Errors the fake network reports with the errno rsockets would use; Plan 9
// lacks the errnos.
var (
	errRefused   error = syscall.NewError("connection refused")
	errAddrInUse error = syscall.NewError("address in use")
	errInvalid   error = syscall.NewError("invalid argument")
	errNotConn   error = syscall.NewError("socket is not connected")
	errConnReset error = syscall.NewError("connection reset by peer")
)
