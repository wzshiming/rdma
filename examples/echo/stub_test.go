//go:build !linux || !cgo

package main

import (
	"errors"
	"io"
	"testing"
)

// TestSendUnsupported checks that without librdmacm the client reports the
// unsupported error instead of hanging or panicking.
func TestSendUnsupported(t *testing.T) {
	err := send("10.200.0.1:4791", "hello", io.Discard)
	if !errors.Is(err, errors.ErrUnsupported) {
		t.Fatalf("send() error = %v, want errors.ErrUnsupported", err)
	}
}
