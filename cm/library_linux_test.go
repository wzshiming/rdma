//go:build linux && !android && (amd64 || arm64 || loong64 || ppc64le || riscv64 || s390x)

package cm

import (
	"context"
	"errors"
	"net/netip"
	"strings"
	"syscall"
	"testing"

	"github.com/wzshiming/rdma/verbs"
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

const missingLibrary = "/nonexistent/librdmacm.so.1"

// TestMissingLibraryIsUnsupported points the package at a librdmacm that
// does not exist: Resolve and Listen fail with a *verbs.OpError matching
// errors.ErrUnsupported and naming librdmacm.so.1.
func TestMissingLibraryIsUnsupported(t *testing.T) {
	useLibrary(t, missingLibrary)
	ctx := context.Background()
	tests := []struct {
		name string
		call func() error
	}{
		{"Resolve", func() error {
			_, err := Resolve(ctx, netip.AddrPort{}, netip.MustParseAddrPort("10.200.0.1:4791"))
			return err
		}},
		{"Listen", func() error {
			_, err := Listen(netip.MustParseAddrPort("10.200.0.1:0"), 1)
			return err
		}},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			err := tt.call()
			var opErr *verbs.OpError
			if !errors.As(err, &opErr) || opErr.Op == "" {
				t.Fatalf("err = %#v, want a *verbs.OpError naming the rdma_cm function", err)
			}
			if !errors.Is(err, errors.ErrUnsupported) {
				t.Errorf("%v does not match errors.ErrUnsupported", err)
			}
			if !strings.Contains(err.Error(), "librdmacm.so.1") {
				t.Errorf("%q does not name librdmacm.so.1", err)
			}
		})
	}
}

func TestLibraryLoadsAfterAFailedAttempt(t *testing.T) {
	useLibrary(t, missingLibrary)
	any := netip.MustParseAddrPort("0.0.0.0:0")
	if _, err := Listen(any, 1); !errors.Is(err, errors.ErrUnsupported) {
		t.Fatalf("Listen() with %s: %v, want errors.ErrUnsupported", missingLibrary, err)
	}
	libMu.Lock()
	libName = "librdmacm.so.1"
	libMu.Unlock()
	// Once loaded, the library answers: with a listener, or with its errno
	// on a host without the rdma_cm module or a device.
	ln, err := Listen(any, 1)
	if err == nil {
		ln.Close()
		return
	}
	var errno syscall.Errno
	if !errors.As(err, &errno) {
		t.Fatalf("Listen() once the library is loadable: %v, want nil or an errno", err)
	}
}
