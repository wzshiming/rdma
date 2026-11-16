//go:build !plan9

package rdma

import "syscall"

// Errors of the socket layer, with the errno rsockets use.
var (
	errAgain     error = syscall.EAGAIN     // nothing to do without blocking
	errPipe      error = syscall.EPIPE      // Write after CloseWrite
	errConnReset error = syscall.ECONNRESET // the connection ended without the peer's end
)
