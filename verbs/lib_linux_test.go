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

// ibv_get_device_index is optional: a library without it loads, and its
// devices report an unknown index.
func TestLibraryWithoutDeviceIndexLoads(t *testing.T) {
	useLibrary(t, "libibverbs.so.1")
	libMu.Lock()
	name := deviceIndexName
	deviceIndexName = "ibv_get_device_index_of_a_newer_library"
	libMu.Unlock()
	t.Cleanup(func() {
		libMu.Lock()
		defer libMu.Unlock()
		deviceIndexName = name
	})
	devs, err := Devices()
	if err != nil {
		var errno syscall.Errno
		if !errors.As(err, &errno) {
			t.Fatalf("Devices() from a library without %s: %v, want nil or an errno", name, err)
		}
	}
	for _, d := range devs {
		if i := d.Index(); i != -1 {
			t.Errorf("Device(%s).Index() from a library without %s = %d, want -1", d.Name(), name, i)
		}
	}
	t.Logf("%d devices report -1", len(devs))
}

// ibv_query_ece and ibv_set_ece are optional: a library without them, older
// than rdma-core 31, loads, and its queue pairs report EOPNOTSUPP, as those
// of a provider without ECE do, without a call into the library.
func TestLibraryWithoutECELoads(t *testing.T) {
	useLibrary(t, "libibverbs.so.1")
	libMu.Lock()
	query, set := queryECEName, setECEName
	queryECEName, setECEName = "ibv_query_ece_of_a_newer_library", "ibv_set_ece_of_a_newer_library"
	libMu.Unlock()
	t.Cleanup(func() {
		libMu.Lock()
		defer libMu.Unlock()
		queryECEName, setECEName = query, set
	})
	if _, err := Devices(); err != nil {
		var errno syscall.Errno
		if !errors.As(err, &errno) {
			t.Fatalf("Devices() from a library without %s and %s: %v, want nil or an errno", query, set, err)
		}
	}
	qp := fakeQP(t, QPTypeRC)
	ece, err := qp.QueryECE()
	var opErr *OpError
	if ece != (ECE{}) || !errors.As(err, &opErr) || opErr.Op != "ibv_query_ece" || !errors.Is(err, syscall.EOPNOTSUPP) {
		t.Errorf("QueryECE() = %+v, %v; want none and an *OpError of ibv_query_ece with EOPNOTSUPP", ece, err)
	}
	if err := qp.SetECE(ECE{VendorID: 0x02c9, Options: 1}); !errors.As(err, &opErr) || opErr.Op != "ibv_set_ece" || !errors.Is(err, syscall.EOPNOTSUPP) {
		t.Errorf("SetECE() error = %v, want an *OpError of ibv_set_ece with EOPNOTSUPP", err)
	}
}

func TestCreateAHWithoutLibrary(t *testing.T) {
	useLibrary(t, missingLibrary)
	_, err := PDFromPointer(nil).CreateAH(&AHAttr{})
	var opErr *OpError
	if !errors.As(err, &opErr) || opErr.Op != "ibv_create_ah" {
		t.Fatalf("CreateAH() error = %v, want an *OpError of ibv_create_ah", err)
	}
	if !errors.Is(err, errors.ErrUnsupported) || !strings.Contains(err.Error(), "libibverbs.so.1") {
		t.Errorf("CreateAH() error = %v, want errors.ErrUnsupported naming libibverbs.so.1", err)
	}
}

func TestMulticastWithoutLibrary(t *testing.T) {
	useLibrary(t, missingLibrary)
	qp := QPFromPointer(nil)
	for op, call := range map[string]func(GID, uint16) error{
		"ibv_attach_mcast": qp.AttachMulticast,
		"ibv_detach_mcast": qp.DetachMulticast,
	} {
		err := call(GID{0: 0xff}, 0)
		var opErr *OpError
		if !errors.As(err, &opErr) || opErr.Op != op || !errors.Is(err, errors.ErrUnsupported) {
			t.Errorf("%s: error = %v, want an *OpError of %s matching errors.ErrUnsupported", op, err, op)
		}
	}
}

// A wrapper of an address handle made elsewhere destroys nothing: its Close
// does not even need the library.
func TestAHFromPointerCloseDestroysNothing(t *testing.T) {
	useLibrary(t, missingLibrary)
	ah := AHFromPointer(cMemory(t, 64))
	if err := ah.Close(); err != nil {
		t.Errorf("Close() error = %v, want nil", err)
	}
	if ah.Pointer() != nil {
		t.Errorf("Pointer() after Close() = %p, want nil", ah.Pointer())
	}
	if err := ah.Close(); err != nil {
		t.Errorf("second Close() error = %v, want nil", err)
	}
}
