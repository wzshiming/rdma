//go:build linux && !android && (amd64 || arm64 || loong64 || ppc64le || riscv64 || s390x)

package main

import (
	"errors"
	"os"
	"testing"

	"github.com/wzshiming/rdma/verbs"
)

// TestDevicesWithoutDevice checks the listing on a Linux host without RDMA
// devices, whether the kernel lacks the RDMA stack or merely the devices. A
// host without libibverbs skips it, or fails it under RDMA_TEST_REQUIRE=1.
func TestDevicesWithoutDevice(t *testing.T) {
	devs, err := verbs.Devices()
	// ENOSYS matches errors.ErrUnsupported too, but means a kernel without
	// the RDMA stack, which the listing must report as no devices.
	if errors.Is(err, errors.ErrUnsupported) && !errors.Is(err, errNoSys) {
		msg := "libibverbs.so.1 cannot be loaded: " + err.Error()
		if os.Getenv("RDMA_TEST_REQUIRE") == "1" {
			t.Fatal(msg + " (RDMA_TEST_REQUIRE=1)")
		}
		t.Skip(msg)
	}
	if err == nil && len(devs) > 0 {
		t.Skipf("host has %d RDMA devices", len(devs))
	}
	code, stdout, stderr := runCLI(t, "devices")
	if code != exitOK || stdout != "no RDMA devices\n" || stderr != "" {
		t.Errorf("run(devices) = %d, stdout %q, stderr %q; want 0, %q, no stderr", code, stdout, stderr, "no RDMA devices\n")
	}
}
