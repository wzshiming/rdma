//go:build !plan9

package rdma

import "syscall"

// Errors reported with the errno a socket would produce.
var (
	errConnReset error = syscall.ECONNRESET // the peer vanished before sending EOF
	errPipe      error = syscall.EPIPE      // Write after CloseWrite
)
