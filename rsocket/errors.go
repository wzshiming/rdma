//go:build !plan9

package rsocket

import (
	"os"
	"syscall"

	"github.com/wzshiming/rdma/verbs"
)

// opError reports an rsockets call op that failed with errno set, as read by
// cgo into err.
func opError(op string, err error) error {
	errno, ok := err.(syscall.Errno)
	if !ok || errno == 0 {
		errno = syscall.EIO
	}
	return &verbs.OpError{Op: op, Err: errno}
}

// errClosed reports op called on a closed Socket.
func errClosed(op string) error { return &verbs.OpError{Op: op, Err: os.ErrClosed} }
