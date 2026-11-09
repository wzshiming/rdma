//go:build !plan9

package cm

import "syscall"

// errConnRefused is what RejectedError matches in errors.Is.
var errConnRefused error = syscall.ECONNREFUSED
