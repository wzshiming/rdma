//go:build !plan9

package rdma

import "syscall"

// Errors the fake fabric reports the way cm would.
var (
	errRefused   error = syscall.ECONNREFUSED
	errAddrInUse error = syscall.EADDRINUSE
)
