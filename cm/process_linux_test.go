//go:build linux && !android && (amd64 || arm64 || loong64 || ppc64le || riscv64 || s390x)

package cm_test

import (
	"context"
	"errors"
	"net/netip"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"

	"github.com/wzshiming/rdma/cm"
	"github.com/wzshiming/rdma/internal/rdmatest"
	"github.com/wzshiming/rdma/verbs"
)

// childEnv marks the child process runInChild starts.
const childEnv = "RDMA_CM_TEST_CHILD"

// runInChild runs the test name alone in a child process, with env added to
// the environment: where a test observes the libraries a fresh process
// loads. It fails t when the child fails, with the child's output.
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

// unloadable returns a library path whose dynamic loader finds an empty file
// named soname first, which fails to load as a missing library would, and
// then searches the directories of this process's LD_LIBRARY_PATH.
func unloadable(t *testing.T, soname string) string {
	t.Helper()
	dir := t.TempDir()
	if err := os.WriteFile(filepath.Join(dir, soname), nil, 0o644); err != nil {
		t.Fatal(err)
	}
	if p := os.Getenv("LD_LIBRARY_PATH"); p != "" {
		dir += ":" + p
	}
	return "LD_LIBRARY_PATH=" + dir
}

// TestConnectsWithoutLibrdmacm connects two ends of the implementation under
// test in a process whose dynamic loader offers an unloadable
// librdmacm.so.1: the reference cannot load it, and the connection is
// established all the same.
func TestConnectsWithoutLibrdmacm(t *testing.T) {
	if os.Getenv(childEnv) == "" {
		rdmatest.Device(t)
		runInChild(t, "TestConnectsWithoutLibrdmacm", unloadable(t, "librdmacm.so.1"))
		return
	}
	dev := rdmatest.Device(t)
	if ln, err := reference.listen(netip.AddrPortFrom(dev.IP, 0), 1); !errors.Is(err, errors.ErrUnsupported) {
		if err == nil {
			ln.Close()
		}
		t.Fatalf("Listen of the reference: %v, want an error matching errors.ErrUnsupported", err)
	}
	_, client, server := pair(t, underTest, underTest)
	if a, b := qpState(t, client.QP()), qpState(t, server.ID.QP()); a != verbs.QPStateRTS || b != verbs.QPStateRTS {
		t.Errorf("queue pairs: client %v, server %v; want both RTS", a, b)
	}
}

// TestUnloadableLibibverbsIsUnsupported runs Resolve and Listen in a process
// whose dynamic loader offers an unloadable libibverbs.so.1: both fail with
// a *verbs.OpError of their operation that matches errors.ErrUnsupported and
// names the library.
func TestUnloadableLibibverbsIsUnsupported(t *testing.T) {
	if os.Getenv(childEnv) == "" {
		runInChild(t, "TestUnloadableLibibverbsIsUnsupported", unloadable(t, "libibverbs.so.1"))
		return
	}
	for _, tt := range []struct {
		name, op string
		call     func() error
	}{
		{"Resolve", "rdma_resolve_addr", func() error {
			_, err := cm.Resolve(context.Background(), netip.AddrPort{}, netip.MustParseAddrPort("10.200.0.1:4791"))
			return err
		}},
		{"Listen", "rdma_listen", func() error {
			_, err := cm.Listen(netip.MustParseAddrPort("10.200.0.1:0"), 1)
			return err
		}},
	} {
		err := tt.call()
		var opErr *verbs.OpError
		if !errors.As(err, &opErr) || opErr.Op != tt.op {
			t.Errorf("%s() error = %#v, want a *verbs.OpError of %s", tt.name, err, tt.op)
		}
		if !errors.Is(err, errors.ErrUnsupported) {
			t.Errorf("%s() error = %v, want one matching errors.ErrUnsupported", tt.name, err)
		}
		if err == nil || !strings.Contains(err.Error(), "libibverbs.so.1") {
			t.Errorf("%s() error = %v, want the text to name libibverbs.so.1", tt.name, err)
		}
		t.Logf("%s() without libibverbs: %v", tt.name, err)
	}
}

// TestUnloadableKeepsTheInheritedPath checks that the child still searches
// the inherited LD_LIBRARY_PATH, where a local build of rdma-core may hold
// the host's only libibverbs.
func TestUnloadableKeepsTheInheritedPath(t *testing.T) {
	t.Setenv("LD_LIBRARY_PATH", "/opt/rdma-core/lib")
	env := unloadable(t, "librdmacm.so.1")
	dir, inherited, _ := strings.Cut(strings.TrimPrefix(env, "LD_LIBRARY_PATH="), ":")
	if _, err := os.Stat(filepath.Join(dir, "librdmacm.so.1")); err != nil || inherited != "/opt/rdma-core/lib" {
		t.Errorf("unloadable() = %q, want LD_LIBRARY_PATH=<the directory of librdmacm.so.1>:/opt/rdma-core/lib", env)
	}
}

// TestUnloadableAddsNoEmptyEntry checks that the child gets no empty entry,
// which the dynamic loader reads as the working directory.
func TestUnloadableAddsNoEmptyEntry(t *testing.T) {
	t.Setenv("LD_LIBRARY_PATH", "")
	env := unloadable(t, "librdmacm.so.1")
	dir := strings.TrimPrefix(env, "LD_LIBRARY_PATH=")
	if _, err := os.Stat(filepath.Join(dir, "librdmacm.so.1")); err != nil {
		t.Errorf("unloadable() = %q, want LD_LIBRARY_PATH=<the directory of librdmacm.so.1>", env)
	}
}
