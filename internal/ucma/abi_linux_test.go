//go:build linux && !android && (amd64 || arm64 || loong64 || ppc64le || riscv64 || s390x)

package ucma

import (
	"errors"
	"os"
	"path/filepath"
	"syscall"
	"testing"
)

// The version check reads sysfs as librdmacm does: class/misc/rdma_cm
// first, then class/infiniband_ucma; with neither it assumes the version it
// speaks. It accepts version 4 only and refuses another with EPERM, as
// librdmacm's initialisation does.
func TestCheckABIVersion(t *testing.T) {
	const misc, ucma = "class/misc/rdma_cm/abi_version", "class/infiniband_ucma/abi_version"
	for _, tt := range []struct {
		name  string
		files map[string]string
		ok    bool
	}{
		{"misc reports 4", map[string]string{misc: "4\n"}, true},
		{"misc reports 3", map[string]string{misc: "3\n"}, false},
		{"misc reports 5", map[string]string{misc: "5\n"}, false},
		{"infiniband_ucma reports 4", map[string]string{ucma: "4\n"}, true},
		{"infiniband_ucma reports 3", map[string]string{ucma: "3\n"}, false},
		{"misc before infiniband_ucma", map[string]string{misc: "4\n", ucma: "3\n"}, true},
		{"neither file", nil, true},
		{"not a number", map[string]string{misc: "four\n"}, false},
	} {
		sysfs := t.TempDir()
		for name, content := range tt.files {
			path := filepath.Join(sysfs, name)
			if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
				t.Fatal(err)
			}
			if err := os.WriteFile(path, []byte(content), 0o644); err != nil {
				t.Fatal(err)
			}
		}
		err := checkABIVersion(sysfs)
		if tt.ok && err != nil {
			t.Errorf("%s: error %v, want none", tt.name, err)
		}
		if !tt.ok && !errors.Is(err, syscall.EPERM) {
			t.Errorf("%s: error %v, want EPERM", tt.name, err)
		}
	}
}
