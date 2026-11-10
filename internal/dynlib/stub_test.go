//go:build !linux || android || !(amd64 || arm64 || loong64 || ppc64le || riscv64 || s390x)

package dynlib_test

import (
	"errors"
	"strings"
	"testing"

	"github.com/wzshiming/rdma/internal/dynlib"
)

func TestOpenUnsupportedPlatform(t *testing.T) {
	const name = "libc.so.6"
	lib, err := dynlib.Open(name)
	if lib != nil || !errors.Is(err, errors.ErrUnsupported) {
		t.Fatalf("Open(%q) = %v, %v; want nil and an error matching errors.ErrUnsupported", name, lib, err)
	}
	if !strings.Contains(err.Error(), name) {
		t.Errorf("Open(%q) = %q, want the library name in the text", name, err)
	}
}
