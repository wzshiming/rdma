//go:build !plan9

package rdmacm_test

import (
	"errors"
	"syscall"
	"testing"

	"github.com/wzshiming/rdma/internal/rdmacm"
)

func TestRejectedError(t *testing.T) {
	err := error(&rdmacm.RejectedError{PrivateData: []byte("busy"), Reason: 28})
	if want := "cm: connection rejected by peer (reason 28)"; err.Error() != want {
		t.Errorf("Error() = %q, want %q", err.Error(), want)
	}
	if !errors.Is(err, syscall.ECONNREFUSED) {
		t.Error("errors.Is(err, syscall.ECONNREFUSED) = false, want true")
	}
	if errors.Is(err, syscall.ECONNRESET) {
		t.Error("errors.Is(err, syscall.ECONNRESET) = true, want false")
	}
	var rej *rdmacm.RejectedError
	if !errors.As(err, &rej) || string(rej.PrivateData) != "busy" {
		t.Errorf("errors.As(err, *RejectedError) failed or data mismatch: %#v", err)
	}
}
