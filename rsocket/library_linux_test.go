//go:build linux && !android && (amd64 || arm64 || loong64 || ppc64le || riscv64 || s390x)

package rsocket

import (
	"errors"
	"strings"
	"syscall"
	"testing"

	"github.com/wzshiming/rdma/verbs"
)

// TestMissingLibraryIsUnsupported points the package at a librdmacm that
// does not exist: New and Poll fail with a *verbs.OpError matching
// errors.ErrUnsupported and naming the library, and nothing panics.
func TestMissingLibraryIsUnsupported(t *testing.T) {
	const name = "librdmacm-missing.so.1"
	defer func(l *loader) { rdmacm = l }(rdmacm)
	rdmacm = &loader{name: name}
	var p [2]int
	if err := syscall.Pipe(p[:]); err != nil {
		t.Fatalf("Pipe() error: %v", err)
	}
	defer syscall.Close(p[0])
	defer syscall.Close(p[1])

	s, err := New(IPv4)
	if s != nil {
		t.Errorf("New() = %v, want nil", s)
	}
	checkUnsupported(t, "New", "rsocket", name, err)
	n, err := Poll([]PollFD{{Fd: p[0], Events: PollIn}}, 0)
	if n != 0 {
		t.Errorf("Poll() = %d, want 0", n)
	}
	checkUnsupported(t, "Poll", "rpoll", name, err)
}

func checkUnsupported(t *testing.T, call, op, lib string, err error) {
	t.Helper()
	var opErr *verbs.OpError
	if !errors.As(err, &opErr) || opErr.Op != op {
		t.Errorf("%s() error = %#v, want a *verbs.OpError with Op %q", call, err, op)
	}
	if !errors.Is(err, errors.ErrUnsupported) {
		t.Errorf("%s() error = %v, want one matching errors.ErrUnsupported", call, err)
	}
	if err == nil || !strings.Contains(err.Error(), lib) {
		t.Errorf("%s() error = %v, want the text to name %s", call, err, lib)
	}
}
