//go:build plan9

package cm

import "syscall"

// errConnRefused is what RejectedError matches in errors.Is; Plan 9 has no
// ECONNREFUSED.
var errConnRefused error = syscall.NewError("connection refused")
