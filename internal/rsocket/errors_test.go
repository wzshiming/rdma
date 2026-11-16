//go:build !plan9

package rsocket

import (
	"errors"
	"os"
	"syscall"
	"testing"

	"github.com/wzshiming/rdma/verbs"
)

func TestErrorShape(t *testing.T) {
	tests := []struct {
		name string
		err  error
		op   string
		is   error
	}{
		{"EAGAIN", opError("raccept", syscall.EAGAIN), "raccept", syscall.EAGAIN},
		{"EINPROGRESS", opError("rconnect", syscall.EINPROGRESS), "rconnect", syscall.EINPROGRESS},
		{"ErrClosed", errClosed("rrecv"), "rrecv", os.ErrClosed},
		{"no errno", opError("rclose", nil), "rclose", syscall.EIO},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			if !errors.Is(tt.err, tt.is) {
				t.Errorf("errors.Is(%v, %v) = false, want true", tt.err, tt.is)
			}
			var opErr *verbs.OpError
			if !errors.As(tt.err, &opErr) || opErr.Op != tt.op {
				t.Errorf("err = %#v, want a *verbs.OpError with Op %q", tt.err, tt.op)
			}
		})
	}
}
