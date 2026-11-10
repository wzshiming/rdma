//go:build !linux || android || !(amd64 || arm64 || loong64 || ppc64le || riscv64 || s390x)

package main

import (
	"errors"
	"io"
	"testing"
)

// TestSendUnsupported checks that on a platform without the bindings the
// client reports the unsupported error instead of hanging or panicking.
func TestSendUnsupported(t *testing.T) {
	err := send("10.200.0.1:4791", "hello", io.Discard)
	if !errors.Is(err, errors.ErrUnsupported) {
		t.Fatalf("send() error = %v, want errors.ErrUnsupported", err)
	}
}
