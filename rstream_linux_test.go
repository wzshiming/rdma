//go:build linux && !android && (amd64 || arm64 || loong64 || ppc64le || riscv64 || s390x)

package rdma

import (
	"bytes"
	"context"
	"errors"
	"io"
	"net"
	"os"
	"os/exec"
	"path/filepath"
	"slices"
	"strconv"
	"strings"
	"syscall"
	"testing"
	"time"

	"github.com/wzshiming/rdma/internal/rdmatest"
)

// The rstream exchange, from run_test, sync_test, send_xfer and recv_xfer in
// rdma-core's librdmacm/examples/rstream.c, run with -I iterations -C
// transfer_count -S transfer_size: first the client sends 16 bytes and the
// server returns 16, then for each iteration the client sends transfer_count
// messages of transfer_size bytes and the server, having received them all,
// sends transfer_count messages of the same size back. A side writes the
// whole batch before it reads, so a server echoing as data arrives would
// fill the client's buffer while the client is still sending; the server
// has to read the batch first. Each side ends with rshutdown(SHUT_RDWR) and
// rclose, and prints a statistics line that starts with the test name,
// "custom" for these flags.
const (
	rstreamIterations = 10
	rstreamCount      = 10
	rstreamSize       = 1000
	rstreamSync       = 16
	rstreamTimeout    = 60 * time.Second
)

// rstreamPath finds the rstream binary of rdmacm-utils, skipping or failing
// the test like rdmatest.Device when it is missing.
func rstreamPath(t testing.TB) string {
	t.Helper()
	rdmatest.Device(t)
	return lookRstream(t)
}

// lookRstream finds rstream on PATH, skipping the test without it, or
// failing it under RDMA_TEST_REQUIRE=1.
func lookRstream(t testing.TB) string {
	t.Helper()
	path, err := exec.LookPath("rstream")
	if err != nil {
		msg := "rstream (rdmacm-utils) not found: " + err.Error()
		if os.Getenv("RDMA_TEST_REQUIRE") == "1" {
			t.Fatal(msg + " (RDMA_TEST_REQUIRE=1)")
		}
		t.Skip(msg)
	}
	return path
}

// startRstream runs rstream with args and returns a function that waits for
// it, checks that it exited 0 and printed the statistics line, and returns
// its standard output.
func startRstream(t testing.TB, path string, args ...string) (wait func() string) {
	t.Helper()
	r := runRstream(t, path, args...)
	return func() string {
		t.Helper()
		err := r.wait()
		t.Logf("rstream %s exited with %v\nstdout:\n%sstderr:\n%s", strings.Join(args, " "), err, r.stdout.String(), r.stderr.String())
		if err != nil {
			t.Errorf("rstream %s: %v", strings.Join(args, " "), err)
		}
		if !strings.Contains(r.stdout.String(), "\ncustom") {
			t.Errorf("rstream %s printed no statistics line", strings.Join(args, " "))
		}
		return r.stdout.String()
	}
}

// rstreamRun is an rstream process of a test.
type rstreamRun struct {
	cmd            *exec.Cmd
	cancel         context.CancelFunc
	stdout, stderr bytes.Buffer
}

// runRstream starts rstream with args, which is killed after rstreamTimeout
// or when t ends.
func runRstream(t testing.TB, path string, args ...string) *rstreamRun {
	t.Helper()
	ctx, cancel := context.WithTimeout(context.Background(), rstreamTimeout)
	r := &rstreamRun{cmd: exec.CommandContext(ctx, path, args...), cancel: cancel}
	r.cmd.Stdout, r.cmd.Stderr = &r.stdout, &r.stderr
	if err := r.cmd.Start(); err != nil {
		cancel()
		t.Fatalf("start rstream %s: %v", strings.Join(args, " "), err)
	}
	t.Cleanup(func() {
		cancel()
		r.cmd.Wait()
	})
	return r
}

// wait waits for rstream to exit and returns the error it exited with.
func (r *rstreamRun) wait() error {
	err := r.cmd.Wait()
	r.cancel()
	return err
}

// rstreamServe plays the server side of the exchange on conn and reads the
// client's EOF.
func rstreamServe(t *testing.T, conn *Conn) {
	t.Helper()
	conn.SetDeadline(time.Now().Add(rstreamTimeout))
	buf := make([]byte, rstreamCount*rstreamSize)
	if _, err := io.ReadFull(conn, buf[:rstreamSync]); err != nil {
		t.Fatalf("sync read: %v", err)
	}
	if _, err := conn.Write(buf[:rstreamSync]); err != nil {
		t.Fatalf("sync write: %v", err)
	}
	for i := range rstreamIterations {
		if _, err := io.ReadFull(conn, buf); err != nil {
			t.Fatalf("iteration %d read: %v", i, err)
		}
		if _, err := conn.Write(buf); err != nil {
			t.Fatalf("iteration %d write: %v", i, err)
		}
	}
	if n, err := io.Copy(io.Discard, conn); err != nil || n != 0 {
		t.Errorf("after the exchange: read %d bytes, %v; want 0 and EOF", n, err)
	}
	if err := conn.Close(); err != nil {
		t.Errorf("Close() error: %v", err)
	}
}

// rstreamClient plays the client side of the exchange on conn.
func rstreamClient(t *testing.T, conn *Conn) {
	t.Helper()
	conn.SetDeadline(time.Now().Add(rstreamTimeout))
	buf := make([]byte, rstreamCount*rstreamSize)
	for i := range buf {
		buf[i] = byte(i)
	}
	if _, err := conn.Write(buf[:rstreamSync]); err != nil {
		t.Fatalf("sync write: %v", err)
	}
	if _, err := io.ReadFull(conn, buf[:rstreamSync]); err != nil {
		t.Fatalf("sync read: %v", err)
	}
	got := make([]byte, len(buf))
	for i := range rstreamIterations {
		if _, err := conn.Write(buf); err != nil {
			t.Fatalf("iteration %d write: %v", i, err)
		}
		if _, err := io.ReadFull(conn, got); err != nil {
			t.Fatalf("iteration %d read: %v", i, err)
		}
	}
	if err := conn.Close(); err != nil {
		t.Errorf("Close() error: %v", err)
	}
}

func rstreamArgs(side string, ip net.IP, port int) []string {
	return rstreamCustomArgs(side, ip, port, rstreamIterations, rstreamCount, rstreamSize)
}

// rstreamCustomArgs are the arguments of rstream's custom test on side, "-s"
// for the client of ip:port and "-b" for the server bound to it: iterations
// of count transfers of size bytes each way.
func rstreamCustomArgs(side string, ip net.IP, port, iterations, count, size int) []string {
	return []string{side, ip.String(), "-p", strconv.Itoa(port),
		"-I", strconv.Itoa(iterations), "-C", strconv.Itoa(count), "-S", strconv.Itoa(size)}
}

// The interop tests run 10 iterations of 10 transfers of 1000 bytes.
func TestRstreamArgs(t *testing.T) {
	got := rstreamArgs("-s", net.IPv4(10, 200, 0, 1), 7471)
	want := []string{"-s", "10.200.0.1", "-p", "7471", "-I", "10", "-C", "10", "-S", "1000"}
	if !slices.Equal(got, want) {
		t.Errorf("rstreamArgs(-s, 10.200.0.1, 7471) = %q, want %q", got, want)
	}
}

// fakeRstream writes an executable named rstream that runs script into a
// directory of its own and returns its path.
func fakeRstream(t *testing.T, script string) string {
	t.Helper()
	path := filepath.Join(t.TempDir(), "rstream")
	if err := os.WriteFile(path, []byte("#!/bin/sh\n"+script), 0o755); err != nil {
		t.Fatal(err)
	}
	return path
}

// The function startRstream returns waits for rstream and returns what it
// printed.
func TestStartRstream(t *testing.T) {
	wait := startRstream(t, fakeRstream(t, "printf 'name\\ncustom\\n'\n"), "-s", "10.200.0.1")
	if out := wait(); out != "name\ncustom\n" {
		t.Errorf("wait() = %q, want what rstream printed", out)
	}
}

// TestRXERstreamClientAgainstListener has rstream's client run the exchange
// against a Listener of each engine.
func TestRXERstreamClientAgainstListener(t *testing.T) {
	forEachEngine(t, testRXERstreamClientAgainstListener)
}

// testRXERstreamClientAgainstListener has rstream's client run the exchange
// against a Listener of the engine e.
func testRXERstreamClientAgainstListener(t *testing.T, e Engine) {
	rstream := rstreamPath(t)
	ln := rxeListen(t, Config{Engine: e})
	addr := ln.Addr().(*Addr)
	wait := startRstream(t, rstream, rstreamArgs("-s", addr.IP, addr.Port)...)
	acc := acceptAsync(ln)
	select {
	case r := <-acc:
		if r.err != nil {
			t.Fatalf("AcceptRDMA() error: %v", r.err)
		}
		rstreamServe(t, r.c)
	case <-time.After(rstreamTimeout):
		t.Fatal("rstream did not connect")
	}
	wait()
}

// TestRXEDialerAgainstRstreamServer has a Dialer of each engine run the
// exchange against rstream's server.
func TestRXEDialerAgainstRstreamServer(t *testing.T) {
	forEachEngine(t, testRXEDialerAgainstRstreamServer)
}

// testRXEDialerAgainstRstreamServer has a Dialer of the engine e run the
// exchange against rstream's server.
func testRXEDialerAgainstRstreamServer(t *testing.T, e Engine) {
	rstream := rstreamPath(t)
	// rstream binds the port it is given, so a free one is found first.
	probe := rxeListen(t, Config{})
	addr := probe.Addr().(*Addr)
	probe.Close()
	wait := startRstream(t, rstream, rstreamArgs("-b", addr.IP, addr.Port)...)
	// rstream's output is buffered until it exits, so the dial is retried
	// until the server listens.
	ctx, cancel := context.WithTimeout(context.Background(), rstreamTimeout)
	defer cancel()
	d := Dialer{Config: Config{Engine: e}}
	var conn *Conn
	for conn == nil {
		c, err := d.DialContext(ctx, "rdma", addr.String())
		switch {
		case err == nil:
			conn = c
		case errors.Is(err, syscall.ECONNREFUSED):
			time.Sleep(50 * time.Millisecond)
		default:
			t.Fatalf("Dial(%v) error: %v", addr, err)
		}
	}
	rstreamClient(t, conn)
	wait()
}
