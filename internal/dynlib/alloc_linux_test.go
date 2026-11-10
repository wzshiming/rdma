//go:build linux && !android && !race && (amd64 || arm64 || loong64 || ppc64le || riscv64 || s390x)

package dynlib_test

import "testing"

// Under the race detector sync.Pool, which purego's SyscallN draws its
// argument block from, drops entries at random, so this test stays out of
// race builds.
func TestCallAllocatesNothing(t *testing.T) {
	getpid := libcFunc(t, "getpid")
	if n := testing.AllocsPerRun(100, func() { getpid.Int(0, 0, 0, 0, 0, 0) }); n != 0 {
		t.Errorf("getpid() allocates %v times per call, want 0", n)
	}
	close := libcFunc(t, "close")
	if n := testing.AllocsPerRun(100, func() { close.Int(cint(-1), 0, 0, 0, 0, 0) }); n != 0 {
		t.Errorf("close(-1), errno included, allocates %v times per call, want 0", n)
	}
}
