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
func rstreamPath(t *testing.T) string {
	t.Helper()
	rdmatest.Device(t)
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
func startRstream(t *testing.T, path string, args ...string) (wait func() string) {
	t.Helper()
	ctx, cancel := context.WithTimeout(context.Background(), rstreamTimeout)
	cmd := exec.CommandContext(ctx, path, args...)
	var stdout, stderr bytes.Buffer
	cmd.Stdout, cmd.Stderr = &stdout, &stderr
	if err := cmd.Start(); err != nil {
		cancel()
		t.Fatalf("start rstream %s: %v", strings.Join(args, " "), err)
	}
	t.Cleanup(func() {
		cancel()
		cmd.Wait()
	})
	return func() string {
		t.Helper()
		err := cmd.Wait()
		cancel()
		t.Logf("rstream %s exited with %v\nstdout:\n%sstderr:\n%s", strings.Join(args, " "), err, stdout.String(), stderr.String())
		if err != nil {
			t.Errorf("rstream %s: %v", strings.Join(args, " "), err)
		}
		if !strings.Contains(stdout.String(), "\ncustom") {
			t.Errorf("rstream %s printed no statistics line", strings.Join(args, " "))
		}
		return stdout.String()
	}
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
	return []string{side, ip.String(), "-p", strconv.Itoa(port),
		"-I", strconv.Itoa(rstreamIterations), "-C", strconv.Itoa(rstreamCount), "-S", strconv.Itoa(rstreamSize)}
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
