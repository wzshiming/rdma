//go:build linux && !android && (amd64 || arm64 || loong64 || ppc64le || riscv64 || s390x)

package ucma

import (
	"fmt"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"syscall"
)

// ABIVersion is the version of the kernel's rdma_cm commands the package
// speaks (RDMA_USER_CM_ABI_VERSION).
const ABIVersion = 4

// CheckABIVersion checks the version the kernel's rdma_cm reports in sysfs,
// as librdmacm does before its first command: class/misc/rdma_cm first,
// then class/infiniband_ucma; with neither, ABIVersion is assumed. Another
// version is an error matching syscall.EPERM, librdmacm's errno for it.
func CheckABIVersion() error { return checkABIVersion("/sys") }

func checkABIVersion(sysfs string) error {
	for _, name := range []string{"class/misc/rdma_cm/abi_version", "class/infiniband_ucma/abi_version"} {
		path := filepath.Join(sysfs, name)
		b, err := os.ReadFile(path)
		if err != nil {
			continue
		}
		text := strings.TrimSpace(string(b))
		if v, err := strconv.Atoi(text); err != nil || v != ABIVersion {
			return fmt.Errorf("ucma: %s reports ABI version %q, want %d: %w", path, text, ABIVersion, syscall.EPERM)
		}
		return nil
	}
	return nil
}
