//go:build linux && !android && (amd64 || arm64 || loong64 || ppc64le || riscv64 || s390x)

package cm

import (
	"context"
	"errors"
	"fmt"
	"net/netip"
	"syscall"
	"testing"

	"github.com/wzshiming/rdma/verbs"
)

// uninitialized makes the package start afresh, its devices listed by list,
// until the test ends.
func uninitialized(t *testing.T, list func() ([]*verbs.Device, error)) {
	t.Helper()
	devMu.Lock()
	defer devMu.Unlock()
	savedList, savedDevices, savedReady := listDevices, devices, initialized
	listDevices, devices, initialized = list, nil, false
	t.Cleanup(func() {
		devMu.Lock()
		defer devMu.Unlock()
		listDevices, devices, initialized = savedList, savedDevices, savedReady
	})
}

// entryPoints are the calls that initialise the package, with the operation
// a libibverbs that cannot be loaded fails them with.
var entryPoints = []struct {
	name, op string
	call     func() error
}{
	{"Resolve", "rdma_resolve_addr", func() error {
		_, err := Resolve(context.Background(), netip.AddrPort{}, netip.MustParseAddrPort("10.200.0.1:4791"))
		return err
	}},
	{"Listen", "rdma_listen", func() error {
		_, err := Listen(netip.MustParseAddrPort("10.200.0.1:0"), 1)
		return err
	}},
}

// TestUnloadableLibibverbsFailsTheEntryPoint pins the error of a libibverbs
// that cannot be loaded: the *verbs.OpError of Resolve's or Listen's own
// operation, as librdmacm's absence gave it, with the loader's error.
func TestUnloadableLibibverbsFailsTheEntryPoint(t *testing.T) {
	loadErr := fmt.Errorf("dynlib: libibverbs.so.1: cannot open shared object file: %w", errors.ErrUnsupported)
	uninitialized(t, func() ([]*verbs.Device, error) {
		return nil, &verbs.OpError{Op: "ibv_get_device_list", Err: loadErr}
	})
	for _, ep := range entryPoints {
		err := ep.call()
		var opErr *verbs.OpError
		if !errors.As(err, &opErr) || opErr.Op != ep.op || opErr.Err != loadErr {
			t.Errorf("%s() error = %#v, want a *verbs.OpError of %s with the loader's error", ep.name, err, ep.op)
		}
		if !errors.Is(err, errors.ErrUnsupported) {
			t.Errorf("%s() error = %v, want one matching errors.ErrUnsupported", ep.name, err)
		}
	}
}

// TestNoDeviceIsENODEV pins the error of a host without a device, as
// librdmacm's initialisation reports it under rdma_create_event_channel:
// an empty list, or a list libibverbs fails to make, even with the ENOSYS
// of a kernel without uverbs, which also matches errors.ErrUnsupported.
func TestNoDeviceIsENODEV(t *testing.T) {
	for _, tt := range []struct {
		name string
		list func() ([]*verbs.Device, error)
	}{
		{"no device", func() ([]*verbs.Device, error) { return nil, nil }},
		{"no uverbs", func() ([]*verbs.Device, error) {
			return nil, &verbs.OpError{Op: "ibv_get_device_list", Err: syscall.ENOSYS}
		}},
	} {
		t.Run(tt.name, func(t *testing.T) {
			uninitialized(t, tt.list)
			for _, ep := range entryPoints {
				err := ep.call()
				var opErr *verbs.OpError
				if !errors.As(err, &opErr) || opErr.Op != "rdma_create_event_channel" || opErr.Err != syscall.ENODEV {
					t.Errorf("%s() error = %#v, want a *verbs.OpError of rdma_create_event_channel with ENODEV", ep.name, err)
				}
				if got, want := fmt.Sprint(err), "verbs: rdma_create_event_channel: no such device"; got != want {
					t.Errorf("%s() error = %q, want %q", ep.name, got, want)
				}
			}
		})
	}
}

// TestInitializationFailureIsNotRemembered checks that a failed
// initialisation is tried again by the next call, as librdmacm's is.
func TestInitializationFailureIsNotRemembered(t *testing.T) {
	calls := 0
	uninitialized(t, func() ([]*verbs.Device, error) {
		calls++
		return nil, nil
	})
	for _, ep := range entryPoints {
		if err := ep.call(); !errors.Is(err, syscall.ENODEV) {
			t.Fatalf("%s() error = %v, want ENODEV", ep.name, err)
		}
	}
	if calls != len(entryPoints) {
		t.Errorf("the devices were listed %d times by %d failing calls, want once each", calls, len(entryPoints))
	}
}
