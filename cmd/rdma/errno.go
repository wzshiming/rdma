//go:build !plan9

package main

import "syscall"

// errNoSys is what libibverbs reports on a kernel without the RDMA stack.
var errNoSys error = syscall.ENOSYS
