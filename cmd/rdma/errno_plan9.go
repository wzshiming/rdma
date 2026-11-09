//go:build plan9

package main

import "syscall"

// errNoSys is what libibverbs reports on a kernel without the RDMA stack;
// Plan 9 has no ENOSYS.
var errNoSys error = syscall.NewError("function not implemented")
