//go:build !linux || android || !(amd64 || arm64 || loong64 || ppc64le || riscv64 || s390x)

package main

import (
	"strings"
	"testing"
)

// TestStubUnsupported checks that on a platform without the bindings every
// command that needs a device reports the unsupported error and fails: that
// of verbs for devices, that of the native engine, which loads no library
// here, for the stream commands, and that of rsocket for them on the
// librdmacm engine.
func TestStubUnsupported(t *testing.T) {
	const noVerbs, noLibrary, noRsocket = "unsupported operation", "shared libraries cannot be called", "rsocket: unsupported operation"
	tests := []struct {
		name string
		args []string
		want string // in stderr
	}{
		{"devices", []string{"devices"}, noVerbs},
		{"pingpong server", []string{"pingpong", "-listen", "10.200.0.1:0"}, noLibrary},
		{"pingpong client", []string{"pingpong", "-connect", "10.200.0.1:4791"}, noLibrary},
		{"bw server", []string{"bw", "-listen", "10.200.0.1:0"}, noLibrary},
		{"bw client", []string{"bw", "-connect", "10.200.0.1:4791", "-duration", "1s"}, noLibrary},
		{"pingpong client on the native engine", []string{"pingpong", "-connect", "10.200.0.1:4791", "-engine", "native"}, noLibrary},
		{"pingpong server on the librdmacm engine", []string{"pingpong", "-listen", "10.200.0.1:0", "-engine", "librdmacm"}, noRsocket},
		{"bw client on the librdmacm engine", []string{"bw", "-connect", "10.200.0.1:4791", "-duration", "1s", "-engine", "librdmacm"}, noRsocket},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			code, stdout, stderr := runCLI(t, tt.args...)
			if code != exitError {
				t.Errorf("run(%q) = %d, want %d", tt.args, code, exitError)
			}
			if !strings.Contains(stderr, tt.want) {
				t.Errorf("stderr = %q, want the unsupported error with %q", stderr, tt.want)
			}
			if stdout != "" {
				t.Errorf("stdout = %q, want nothing", stdout)
			}
		})
	}
}
