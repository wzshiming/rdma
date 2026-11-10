//go:build linux && !android && (amd64 || arm64 || loong64 || ppc64le || riscv64 || s390x)

package rdma

import (
	"errors"
	"net"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
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

// TestMissingLibraryIsUnsupported runs Dial and Listen in a process whose
// dynamic loader finds an empty file in place of librdmacm.so.1, which
// fails to load as a missing one would: the process starts, and both
// return a *net.OpError matching errors.ErrUnsupported that names the
// library.
func TestMissingLibraryIsUnsupported(t *testing.T) {
	if os.Getenv(childEnv) == "" {
		dir := t.TempDir()
		if err := os.WriteFile(filepath.Join(dir, "librdmacm.so.1"), nil, 0o644); err != nil {
			t.Fatal(err)
		}
		runInChild(t, "TestMissingLibraryIsUnsupported", "LD_LIBRARY_PATH="+dir)
		return
	}
	for _, tt := range []struct {
		name, op string
		call     func() error
	}{
		{"Dial", "dial", func() error { _, err := Dial("rdma", "10.200.0.1:4791"); return err }},
		{"Listen", "listen", func() error { _, err := Listen("rdma", "10.200.0.1:0"); return err }},
	} {
		err := tt.call()
		var opErr *net.OpError
		if !errors.As(err, &opErr) || opErr.Op != tt.op || opErr.Net != "rdma" {
			t.Errorf("%s() error = %#v, want a *net.OpError with Op %q on net rdma", tt.name, err, tt.op)
		}
		if !errors.Is(err, errors.ErrUnsupported) {
			t.Errorf("%s() error = %v, want one matching errors.ErrUnsupported", tt.name, err)
		}
		if err == nil || !strings.Contains(err.Error(), "librdmacm.so.1") {
			t.Errorf("%s() error = %v, want the text to name librdmacm.so.1", tt.name, err)
		}
		t.Logf("%s() without librdmacm: %v", tt.name, err)
	}
}
