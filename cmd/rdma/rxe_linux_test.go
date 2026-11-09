//go:build linux && cgo

package main

import (
	"bufio"
	"context"
	"errors"
	"fmt"
	"io"
	"net"
	"os"
	"os/exec"
	"path/filepath"
	"runtime"
	"strconv"
	"strings"
	"testing"
	"time"

	"github.com/wzshiming/rdma/internal/rdmatest"
)

const smokeTimeout = 30 * time.Second

// TestRXECLI builds the command and runs it against itself on the rdmatest
// device: the server side listens on port 0 and the client side connects
// to the address the server prints.
func TestRXECLI(t *testing.T) {
	dev := rdmatest.Device(t)
	bin := buildCLI(t)
	ip := dev.IP.String()

	t.Run("devices", func(t *testing.T) {
		out := runBinary(t, bin, "devices")
		for _, want := range []string{"device: " + dev.Name + "\n", "state: ACTIVE\n", "link layer: ", "mtu: "} {
			if !strings.Contains(out, want) {
				t.Errorf("devices output lacks %q:\n%s", want, out)
			}
		}
		// RoCE lists the IP as a GID; InfiniBand GIDs are unrelated to it and
		// iWARP (GIDIndex -1) addresses peers by IP alone.
		if dev.GIDIndex >= 0 {
			want := fmt.Sprintf("gid %d: ", dev.GIDIndex)
			if strings.Contains(out, "link layer: Ethernet\n") {
				want = fmt.Sprintf("gid %d: ::ffff:%s (RoCE v2)\n", dev.GIDIndex, ip)
			}
			if !strings.Contains(out, want) {
				t.Errorf("devices output lacks %q:\n%s", want, out)
			}
		}
	})

	t.Run("pingpong", func(t *testing.T) {
		addr, wait := startServer(t, bin, "pingpong", "-listen", net.JoinHostPort(ip, "0"))
		out := runBinary(t, bin, "pingpong", "-connect", addr, "-n", "1000", "-size", "64")
		wait(t)
		f := fields(t, out)
		if f["iterations"] != "1000" || f["size"] != "64" {
			t.Errorf("iterations = %q, size = %q, want 1000 and 64", f["iterations"], f["size"])
		}
		var prev time.Duration
		for _, k := range []string{"min", "median", "p99", "max"} {
			d, err := time.ParseDuration(f[k])
			if err != nil || d <= 0 || d < prev {
				t.Errorf("%s = %q (%v), want a positive duration of at least %v", k, f[k], err, prev)
			}
			prev = d
		}
		if rate, err := strconv.ParseFloat(f["msg/s"], 64); err != nil || rate <= 0 {
			t.Errorf("msg/s = %q (%v), want a positive number", f["msg/s"], err)
		}
	})

	t.Run("bw", func(t *testing.T) {
		addr, wait := startServer(t, bin, "bw", "-listen", net.JoinHostPort(ip, "0"))
		out := runBinary(t, bin, "bw", "-connect", addr, "-duration", "2s")
		wait(t)
		f := fields(t, out)
		if f["connections"] != "1" || f["size"] != "65536" {
			t.Errorf("connections = %q, size = %q, want 1 and 65536", f["connections"], f["size"])
		}
		if n, err := strconv.ParseInt(f["bytes"], 10, 64); err != nil || n <= 0 {
			t.Errorf("bytes = %q (%v), want a positive count", f["bytes"], err)
		}
		if d, err := time.ParseDuration(f["duration"]); err != nil || d < 2*time.Second {
			t.Errorf("duration = %q (%v), want at least 2s", f["duration"], err)
		}
		for _, k := range []string{"MiB/s", "Gbit/s"} {
			if rate, err := strconv.ParseFloat(f[k], 64); err != nil || rate <= 0 {
				t.Errorf("%s = %q (%v), want a positive number", k, f[k], err)
			}
		}
	})

	t.Run("bw with two connections", func(t *testing.T) {
		addr, wait := startServer(t, bin, "bw", "-listen", net.JoinHostPort(ip, "0"), "-conns", "2")
		out := runBinary(t, bin, "bw", "-connect", addr, "-conns", "2", "-duration", "1s", "-size", "4096")
		wait(t)
		f := fields(t, out)
		if f["connections"] != "2" || f["size"] != "4096" {
			t.Errorf("connections = %q, size = %q, want 2 and 4096", f["connections"], f["size"])
		}
		for _, k := range []string{"connection 0", "connection 1"} {
			if !strings.HasPrefix(f[k], "bytes ") {
				t.Errorf("%s = %q, want a per-connection line", k, f[k])
			}
		}
	})
}

// buildCLI builds the command into a temporary directory.
func buildCLI(t *testing.T) string {
	t.Helper()
	goTool, err := exec.LookPath("go")
	if err != nil {
		goTool = filepath.Join(runtime.GOROOT(), "bin", "go")
	}
	bin := filepath.Join(t.TempDir(), "rdma")
	cmd := exec.Command(goTool, "build", "-o", bin, ".")
	if out, err := cmd.CombinedOutput(); err != nil {
		t.Fatalf("go build: %v\n%s", err, out)
	}
	return bin
}

// runBinary runs the command with args and returns its standard output,
// failing the test unless it exits 0.
func runBinary(t *testing.T, bin string, args ...string) string {
	t.Helper()
	ctx, cancel := context.WithTimeout(context.Background(), smokeTimeout)
	defer cancel()
	out, err := exec.CommandContext(ctx, bin, args...).Output()
	if err != nil {
		var exitErr *exec.ExitError
		if errors.As(err, &exitErr) {
			t.Fatalf("rdma %s exited %d: %s", strings.Join(args, " "), exitErr.ExitCode(), exitErr.Stderr)
		}
		t.Fatalf("rdma %s: %v", strings.Join(args, " "), err)
	}
	t.Logf("rdma %s:\n%s", strings.Join(args, " "), out)
	return string(out)
}

// startServer starts the command as a server, returns the address of its
// "listening on" line and a function that waits for it to exit 0.
func startServer(t *testing.T, bin string, args ...string) (addr string, wait func(t *testing.T)) {
	t.Helper()
	ctx, cancel := context.WithTimeout(context.Background(), smokeTimeout)
	t.Cleanup(cancel)
	cmd := exec.CommandContext(ctx, bin, args...)
	cmd.Stderr = os.Stderr
	stdout, err := cmd.StdoutPipe()
	if err != nil {
		t.Fatal(err)
	}
	if err := cmd.Start(); err != nil {
		t.Fatalf("rdma %s: %v", strings.Join(args, " "), err)
	}
	waited := false
	t.Cleanup(func() {
		if !waited {
			cmd.Process.Kill()
			cmd.Wait()
		}
	})
	r := bufio.NewReader(stdout)
	line, err := r.ReadString('\n')
	if err != nil || !strings.HasPrefix(line, "listening on ") {
		t.Fatalf("rdma %s: first line %q, %v; want \"listening on ip:port\"", strings.Join(args, " "), line, err)
	}
	addr = strings.TrimSpace(strings.TrimPrefix(line, "listening on "))
	if _, port, err := net.SplitHostPort(addr); err != nil || port == "0" {
		t.Fatalf("rdma %s: listening on %q, want an address with the port picked", strings.Join(args, " "), addr)
	}
	return addr, func(t *testing.T) {
		t.Helper()
		rest, _ := io.ReadAll(r)
		waited = true
		if err := cmd.Wait(); err != nil {
			t.Errorf("rdma %s: %v", strings.Join(args, " "), err)
		}
		if len(rest) > 0 {
			t.Errorf("rdma %s printed after the listening line: %q", strings.Join(args, " "), rest)
		}
	}
}

// fields parses "key: value" lines.
func fields(t *testing.T, out string) map[string]string {
	t.Helper()
	f := make(map[string]string)
	for _, line := range strings.Split(strings.TrimSpace(out), "\n") {
		k, v, ok := strings.Cut(line, ": ")
		if !ok {
			t.Errorf("line %q is not \"key: value\"", line)
			continue
		}
		f[k] = v
	}
	return f
}
