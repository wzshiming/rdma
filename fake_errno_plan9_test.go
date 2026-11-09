//go:build plan9

package rdma

import "syscall"

// Errors the fake fabric reports the way cm would; Plan 9 lacks the errnos.
var (
	errRefused   error = syscall.NewError("connection refused")
	errAddrInUse error = syscall.NewError("address in use")
)
