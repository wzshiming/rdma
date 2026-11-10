//go:build linux && !android && (amd64 || arm64 || loong64 || ppc64le || riscv64 || s390x)

package verbs

import (
	"errors"
	"strings"
	"syscall"
	"testing"
)

// useLibrary makes the next use of the package open the library at name,
// whatever it has loaded already, until the test ends.
func useLibrary(t *testing.T, name string) {
	t.Helper()
	libMu.Lock()
	defer libMu.Unlock()
	saved, savedName := lib.Swap(nil), libName
	libName = name
	t.Cleanup(func() {
		libMu.Lock()
		defer libMu.Unlock()
		libName = savedName
		lib.Store(saved)
	})
}

const missingLibrary = "/nonexistent/libibverbs.so.1"

func TestEntryPointsWithoutLibrary(t *testing.T) {
	useLibrary(t, missingLibrary)
	tests := []struct {
		name string
		call func() error
	}{
		{"Devices", func() error { _, err := Devices(); return err }},
		{"ForkInit", ForkInit},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			err := tt.call()
			var opErr *OpError
			if !errors.As(err, &opErr) {
				t.Fatalf("err = %T (%v), want *OpError", err, err)
			}
			if !errors.Is(err, errors.ErrUnsupported) {
				t.Errorf("%v does not match errors.ErrUnsupported", err)
			}
			if errors.Is(err, syscall.ENOSYS) {
				t.Errorf("%v matches ENOSYS, which means a host without devices", err)
			}
			if !strings.Contains(err.Error(), "libibverbs.so.1") {
				t.Errorf("%q does not name libibverbs.so.1", err)
			}
		})
	}
}

func TestLibraryLoadsAfterAFailedAttempt(t *testing.T) {
	useLibrary(t, missingLibrary)
	if _, err := Devices(); !errors.Is(err, errors.ErrUnsupported) {
		t.Fatalf("Devices() with %s: %v, want errors.ErrUnsupported", missingLibrary, err)
	}
	libMu.Lock()
	libName = "libibverbs.so.1"
	libMu.Unlock()
	// Once loaded, the library answers: with devices, or with its errno
	// (ENOSYS on a kernel without the RDMA stack).
	if _, err := Devices(); err != nil {
		var errno syscall.Errno
		if !errors.As(err, &errno) {
			t.Fatalf("Devices() once the library is loadable: %v, want nil or an errno", err)
		}
	}
}
