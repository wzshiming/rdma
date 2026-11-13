//go:build linux && !android && (amd64 || arm64 || loong64 || ppc64le || riscv64 || s390x)

package rdma

import (
	"net"
	"path/filepath"
	"slices"
	"strconv"
	"strings"
	"testing"
	"time"

	"github.com/wzshiming/rdma/internal/rsproto"
)

// benchRstream runs b.N ops of op on transfers of size bytes between two
// rstreams on the device, with the buffers of cfg, and reports the time and
// the throughput of an op from the statistics of rstream's client, which
// times its transfers only.
func benchRstream(b *testing.B, op rstreamOp, cfg Config, size int) {
	path := rstreamPath(b)
	// rstream binds the port it is given, so a free one is found first.
	probe := rxeListen(b, Config{})
	addr := probe.Addr().(*Addr)
	probe.Close()
	b.ResetTimer()
	server := runRstream(b, path, rstreamBenchArgs("-b", addr.IP, addr.Port, op, b.N, size, cfg)...)
	out := rstreamClientOutput(b, path, rstreamBenchArgs("-s", addr.IP, addr.Port, op, b.N, size, cfg))
	if err := server.wait(); err != nil {
		b.Fatalf("rstream's server: %v\nstdout:\n%sstderr:\n%s", err, server.stdout.String(), server.stderr.String())
	}
	b.StopTimer()
	ns, mbps, err := rstreamPerOp(out, op, size)
	if err != nil {
		b.Fatal(err)
	}
	b.ReportMetric(ns, "ns/op")
	b.ReportMetric(mbps, "MB/s")
}

// rstreamBenchArgs are the arguments of the rstream on side, "-s" for the
// client of ip:port and "-b" for the server bound to it, that runs n ops of
// op on transfers of size bytes: blocking (-T b), as the engines' Read and
// Write block, and with the send buffer of cfg (-B), which rstream gives
// SO_SNDBUF and SO_RCVBUF alike and rsockets double as the engines do. A
// custom test leaves RDMA_INLINE at librdmacm's default, 64, the engines'
// MaxInline by default.
func rstreamBenchArgs(side string, ip net.IP, port int, op rstreamOp, n, size int, cfg Config) []string {
	iterations, transfers := n, 1
	if op == rstreamStream {
		iterations, transfers = 1, n
	}
	buffer := cfg.SendBufferSize
	if buffer == 0 {
		buffer = rsproto.DefaultSendBufferSize / 2
	}
	return append(rstreamCustomArgs(side, ip, port, iterations, transfers, size), "-T", "b", "-B", strconv.Itoa(buffer))
}

// rstreamClientOutput runs rstream's client with args until it connects and
// returns what it printed. Its server prints nothing until it exits, so the
// client starts again while its connection is refused, as it is until the
// server listens.
func rstreamClientOutput(t testing.TB, path string, args []string) string {
	t.Helper()
	deadline := time.Now().Add(rstreamTimeout)
	for {
		client := runRstream(t, path, args...)
		err := client.wait()
		if err == nil {
			return client.stdout.String()
		}
		if !strings.Contains(client.stderr.String(), "rconnect: Connection refused") || time.Now().After(deadline) {
			t.Fatalf("rstream %s: %v\nstdout:\n%sstderr:\n%s", strings.Join(args, " "), err, client.stdout.String(), client.stderr.String())
		}
		time.Sleep(10 * time.Millisecond)
	}
}

// Both rstreams block, as the engines' Read and Write do, with the buffers of
// the row's Config, 128 KiB registered for the zero one; a round trip is an
// iteration of a transfer each way, a stream one iteration of all.
func TestRstreamBenchArgs(t *testing.T) {
	ip := net.IPv4(10, 200, 0, 1)
	for _, tt := range []struct {
		name string
		side string
		op   rstreamOp
		cfg  Config
		want []string
	}{
		{"round trip", "-s", rstreamRoundTrip, Config{},
			[]string{"-s", "10.200.0.1", "-p", "7471", "-I", "100", "-C", "1", "-S", "64", "-T", "b", "-B", "65536"}},
		{"stream with large buffers", "-b", rstreamStream, Config{SendBufferSize: 1 << 20, ReceiveBufferSize: 1 << 20},
			[]string{"-b", "10.200.0.1", "-p", "7471", "-I", "1", "-C", "100", "-S", "64", "-T", "b", "-B", "1048576"}},
	} {
		t.Run(tt.name, func(t *testing.T) {
			if got := rstreamBenchArgs(tt.side, ip, 7471, tt.op, 100, 64, tt.cfg); !slices.Equal(got, tt.want) {
				t.Errorf("rstreamBenchArgs = %q, want %q", got, tt.want)
			}
		})
	}
}

// rstream's client starts again while its connection is refused, until it
// connects, and fails at once on any other error.
func TestRstreamClientOutput(t *testing.T) {
	t.Run("refused once", func(t *testing.T) {
		path := fakeRstream(t, `if [ ! -e "$0.refused" ]; then
	: > "$0.refused"
	echo 'rconnect: Connection refused' >&2
	exit 255
fi
printf 'name\ncustom\n'
`)
		if out := rstreamClientOutput(t, path, nil); out != "name\ncustom\n" {
			t.Errorf("rstreamClientOutput() = %q, want what the second client printed", out)
		}
	})
	t.Run("reset", func(t *testing.T) {
		path := fakeRstream(t, "echo 'rrecv: Connection reset by peer' >&2\nexit 255\n")
		if _, failed := benchOutcome(func(b *testing.B) { rstreamClientOutput(b, path, nil) }); !failed {
			t.Error("rstreamClientOutput() of a client that lost its connection did not fail")
		}
	})
}

// Without rstream on PATH, the rstream column skips, or fails under
// RDMA_TEST_REQUIRE=1; with it, lookRstream returns its path.
func TestLookRstream(t *testing.T) {
	for _, tt := range []struct {
		require            string
		wantSkip, wantFail bool
	}{
		{"", true, false},
		{"1", false, true},
	} {
		t.Run("RDMA_TEST_REQUIRE="+tt.require, func(t *testing.T) {
			t.Setenv("PATH", t.TempDir())
			t.Setenv("RDMA_TEST_REQUIRE", tt.require)
			skipped, failed := benchOutcome(func(b *testing.B) { lookRstream(b) })
			if skipped != tt.wantSkip || failed != tt.wantFail {
				t.Errorf("without rstream: skipped=%v failed=%v, want skipped=%v failed=%v", skipped, failed, tt.wantSkip, tt.wantFail)
			}
		})
	}
	t.Run("on PATH", func(t *testing.T) {
		path := fakeRstream(t, "")
		t.Setenv("PATH", filepath.Dir(path))
		if got := lookRstream(t); got != path {
			t.Errorf("lookRstream() = %q, want %q", got, path)
		}
	})
}

// benchOutcome runs bench as a benchmark of its own, as testing.Benchmark
// does, and reports whether it skipped or failed.
func benchOutcome(bench func(b *testing.B)) (skipped, failed bool) {
	testing.Benchmark(func(b *testing.B) {
		defer func() { skipped, failed = b.Skipped(), b.Failed() }()
		bench(b)
	})
	return skipped, failed
}
