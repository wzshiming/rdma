//go:build !plan9

package rdma

import "syscall"

// Errors the fake network reports with the errno rsockets would use.
var (
	errRefused   error = syscall.ECONNREFUSED
	errAddrInUse error = syscall.EADDRINUSE
	errInvalid   error = syscall.EINVAL
	errNotConn   error = syscall.ENOTCONN
)

// errInProgress reports a connect that goes on in the background, as rsockets
// and the fake network do.
var errInProgress error = syscall.EINPROGRESS
