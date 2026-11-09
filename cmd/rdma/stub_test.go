//go:build !linux || !cgo

package main

import (
	"strings"
	"testing"
)

// TestStubUnsupported checks that without rdma-core every command that needs
// a device reports the unsupported error and fails.
func TestStubUnsupported(t *testing.T) {
	tests := []struct {
		name string
		args []string
	}{
		{"devices", []string{"devices"}},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			code, stdout, stderr := runCLI(t, tt.args...)
			if code != exitError {
				t.Errorf("run(%q) = %d, want %d", tt.args, code, exitError)
			}
			if !strings.Contains(stderr, "unsupported operation") {
				t.Errorf("stderr = %q, want the unsupported error", stderr)
			}
			if stdout != "" {
				t.Errorf("stdout = %q, want nothing", stdout)
			}
		})
	}
}
