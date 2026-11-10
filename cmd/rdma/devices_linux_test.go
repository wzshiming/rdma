//go:build linux && !android && (amd64 || arm64 || loong64 || ppc64le || riscv64 || s390x)

package main

import (
	"testing"

	"github.com/wzshiming/rdma/verbs"
)

// TestDevicesWithoutDevice checks the listing on a Linux host without RDMA
// devices, whether the kernel lacks the RDMA stack or merely the devices.
func TestDevicesWithoutDevice(t *testing.T) {
	if devs, err := verbs.Devices(); err == nil && len(devs) > 0 {
		t.Skipf("host has %d RDMA devices", len(devs))
	}
	code, stdout, stderr := runCLI(t, "devices")
	if code != exitOK || stdout != "no RDMA devices\n" || stderr != "" {
		t.Errorf("run(devices) = %d, stdout %q, stderr %q; want 0, %q, no stderr", code, stdout, stderr, "no RDMA devices\n")
	}
}
