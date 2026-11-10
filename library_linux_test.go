//go:build linux && !android && (amd64 || arm64 || loong64 || ppc64le || riscv64 || s390x)

package rdma

import (
	"errors"
	"os"
	"os/exec"
	"testing"

	"github.com/wzshiming/rdma/rsocket"
)

// childEnv marks the child process runInChild starts.
const childEnv = "RDMA_TEST_CHILD"

// runInChild runs the test name alone in a child process, with env added to
// the environment: where a test observes the process's first use of
// librdmacm, which this process may have made already. It fails t when the
// child fails, with the child's output.
func runInChild(t *testing.T, name string, env ...string) {
	t.Helper()
	cmd := exec.Command(os.Args[0], "-test.run=^"+name+"$", "-test.v", "-test.count=1")
	cmd.Env = append(append(os.Environ(), childEnv+"=1"), env...)
	out, err := cmd.CombinedOutput()
	t.Logf("child process:\n%s", out)
	if err != nil {
		t.Fatalf("child process: %v", err)
	}
}

// requireLibrary skips t, or fails it under RDMA_TEST_REQUIRE=1, when the
// dynamic loader finds no librdmacm.so.1 on this host.
func requireLibrary(t *testing.T) {
	t.Helper()
	s, err := rsocket.New(rsocket.IPv4)
	if err == nil {
		s.Close()
		return
	}
	if errors.Is(err, errors.ErrUnsupported) {
		msg := "librdmacm.so.1 cannot be loaded: " + err.Error()
		if os.Getenv("RDMA_TEST_REQUIRE") == "1" {
			t.Fatal(msg + " (RDMA_TEST_REQUIRE=1)")
		}
		t.Skip(msg)
	}
}

// TestRdmaCoreReleaseLoadsTheLibrary checks, in a process that has not used
// librdmacm yet, that rdmaCoreRelease reports its release: the library is
// loaded on first use, not at start-up, and a release read before that
// would be 0, an unknown release that lets the many-connection tests run on
// an rdma-core that corrupts its service threads.
func TestRdmaCoreReleaseLoadsTheLibrary(t *testing.T) {
	if os.Getenv(childEnv) == "" {
		requireLibrary(t)
		runInChild(t, "TestRdmaCoreReleaseLoadsTheLibrary")
		return
	}
	v := rdmaCoreRelease()
	if v == 0 {
		t.Fatal("rdmaCoreRelease() = 0 in a fresh process: librdmacm is not loaded before its release is read")
	}
	t.Logf("rdma-core release %d", v)
}
