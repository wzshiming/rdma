//go:build linux && !android && (amd64 || arm64 || loong64 || ppc64le || riscv64 || s390x)

package rdma

import (
	"bytes"
	"context"
	"errors"
	"io"
	"net"
	"os"
	"runtime"
	"strconv"
	"strings"
	"sync"
	"syscall"
	"testing"
	"time"

	"golang.org/x/net/nettest"

	"github.com/wzshiming/rdma/internal/rdmatest"
)

const rxeTimeout = 20 * time.Second

// smallConfig sizes the buffers and queues well below the rsockets defaults,
// so that back-pressure and credit returns happen all the time.
var smallConfig = Config{SendBufferSize: 16 << 10, ReceiveBufferSize: 16 << 10, SendQueueSize: 8, ReceiveQueueSize: 8}

// manyConnections gates a test that keeps more than three connections alive
// in this process by the rdma-core release in use, as manyConnectionsVerdict
// decides: a skip before 53, a failure under RDMA_TEST_REQUIRE=1, and a run
// under RDMA_TEST_MANY_CONNS=1 regardless.
func manyConnections(t *testing.T) {
	t.Helper()
	rdmatest.Device(t)
	release := rdmaCoreRelease()
	msg := "rdma-core " + strconv.Itoa(release) + " corrupts its service threads beyond three registered rsockets (fixed in 53); RDMA_TEST_MANY_CONNS=1 runs the test anyway"
	switch manyConnectionsVerdict(release, os.Getenv) {
	case verdictSkip:
		t.Skip(msg)
	case verdictFatal:
		t.Fatal(msg + " (RDMA_TEST_REQUIRE=1)")
	}
}

// rdmaCoreRelease reads the rdma-core release from the file name of the
// librdmacm this process has loaded, librdmacm.so.1.<minor>.<release>.<patch>,
// which is the library the tests run against whichever directory holds it;
// it returns 0 when none is mapped.
func rdmaCoreRelease() int {
	maps, err := os.ReadFile("/proc/self/maps")
	if err != nil {
		return 0
	}
	return rdmaCoreReleaseIn(string(maps))
}

// TestRXELibraryRelease records the rdma-core release the tests run against.
func TestRXELibraryRelease(t *testing.T) {
	v := rdmaCoreRelease()
	if v == 0 {
		t.Skip("librdmacm.so.1 not mapped")
	}
	t.Logf("rdma-core release %d", v)
}

// rxeListen starts a Listener on port 0 of the rdmatest device's IP.
func rxeListen(t testing.TB, cfg Config) *Listener {
	t.Helper()
	dev := rdmatest.Device(t)
	lc := ListenConfig{Config: cfg}
	ln, err := lc.Listen(context.Background(), "rdma", net.JoinHostPort(dev.IP.String(), "0"))
	if err != nil {
		t.Fatalf("Listen(%v:0) error: %v", dev.IP, err)
	}
	t.Cleanup(func() { ln.Close() })
	return ln
}

// rxeDial dials addr with cfg within rxeTimeout.
func rxeDial(t testing.TB, cfg Config, addr net.Addr) *Conn {
	t.Helper()
	d := Dialer{Config: cfg, Timeout: rxeTimeout}
	c, err := d.Dial("rdma", addr.String())
	if err != nil {
		t.Fatalf("Dial(%v) error: %v", addr, err)
	}
	t.Cleanup(func() { c.Close() })
	return c
}

// rxePair connects a client to a fresh listener on the device and closes the
// listener: rdma-core before 53 (24061f015bd9) polls a stale descriptor array
// in its listen thread once more than three listeners are registered, so
// none is kept open longer than needed.
func rxePair(t testing.TB, cfg Config) (client, server *Conn) {
	t.Helper()
	ln := rxeListen(t, cfg)
	acc := acceptAsync(ln)
	client = rxeDial(t, cfg, ln.Addr())
	r := <-acc
	if r.err != nil {
		t.Fatalf("AcceptRDMA() error: %v", r.err)
	}
	t.Cleanup(func() { r.c.Close() })
	if err := ln.Close(); err != nil {
		t.Fatalf("Listener Close() error: %v", err)
	}
	return client, r.c
}

func TestRXEConn(t *testing.T) {
	rdmatest.Device(t)
	nettest.TestConn(t, func() (c1, c2 net.Conn, stop func(), err error) {
		client, server := rxePair(t, Config{})
		return client, server, func() {
			client.Close()
			server.Close()
		}, nil
	})
}

func TestRXEEcho(t *testing.T) {
	client, server := rxePair(t, smallConfig)
	go io.Copy(server, server)
	for _, n := range []int{1, 100, 16383, 16384, 16385, 3*16384 + 5, 1000000} {
		want := make([]byte, n)
		newStream(byte(n)).rng.Read(want)
		werr := make(chan error, 1)
		go func() {
			_, err := client.Write(want)
			werr <- err
		}()
		got := make([]byte, n)
		if _, err := io.ReadFull(client, got); err != nil {
			t.Fatalf("ReadFull(%d bytes) error: %v", n, err)
		}
		if err := <-werr; err != nil {
			t.Fatalf("Write(%d bytes) error: %v", n, err)
		}
		if !bytes.Equal(got, want) {
			t.Errorf("echo of %d bytes differs", n)
		}
	}
}

func TestRXETransfer(t *testing.T) {
	rdmatest.Device(t)
	for name, cfg := range map[string]Config{"defaults": {}, "small": smallConfig} {
		t.Run(name, func(t *testing.T) {
			client, server := rxePair(t, cfg)
			const total = 1 << 30
			start := time.Now()
			transfer(t, client, server, total, 64<<10, client.CloseWrite)
			elapsed := time.Since(start)
			t.Logf("%d MiB in %v (%.0f MiB/s)", total>>20, elapsed.Round(time.Millisecond), float64(total>>20)/elapsed.Seconds())
		})
	}
}

func TestRXEConcurrentConnections(t *testing.T) {
	manyConnections(t)
	ln := rxeListen(t, Config{})
	const conns = 32
	const total = 4 << 20
	var wg sync.WaitGroup
	for range conns {
		wg.Add(1)
		go func() {
			defer wg.Done()
			server, err := ln.AcceptRDMA()
			if err != nil {
				if !errors.Is(err, net.ErrClosed) {
					t.Errorf("AcceptRDMA() error: %v", err)
				}
				return
			}
			defer server.Close()
			if _, err := io.Copy(server, server); err != nil {
				t.Errorf("echo error: %v", err)
			}
			server.CloseWrite()
		}()
	}
	for i := range conns {
		wg.Add(1)
		go func() {
			defer wg.Done()
			d := Dialer{Timeout: rxeTimeout}
			client, err := d.Dial("rdma", ln.Addr().String())
			if err != nil {
				t.Errorf("Dial() #%d error: %v", i, err)
				ln.Close() // frees the acceptor waiting for this dial
				return
			}
			defer client.Close()
			in, out := newStream(byte(i)), newStream(byte(i))
			var rwg sync.WaitGroup
			rwg.Add(1)
			go func() {
				defer rwg.Done()
				if err := out.readFrom(client, 32<<10); err != nil {
					t.Errorf("Read #%d error: %v", i, err)
				}
			}()
			if err := in.writeTo(client, total, 32<<10); err != nil {
				t.Errorf("Write #%d error: %v", i, err)
			}
			if err := client.CloseWrite(); err != nil {
				t.Errorf("CloseWrite #%d error: %v", i, err)
			}
			rwg.Wait()
			if in.sum() != out.sum() {
				t.Errorf("connection #%d: sent %s, received %s", i, in.sum(), out.sum())
			}
		}()
	}
	wg.Wait()
}

func TestRXEListenerCloseUnblocksAccept(t *testing.T) {
	ln := rxeListen(t, Config{})
	errc := make(chan error, 1)
	go func() {
		_, err := ln.Accept()
		errc <- err
	}()
	time.Sleep(100 * time.Millisecond)
	if err := ln.Close(); err != nil {
		t.Fatalf("Close() error: %v", err)
	}
	select {
	case err := <-errc:
		if !errors.Is(err, net.ErrClosed) {
			t.Errorf("Accept() after Close() error = %v, want net.ErrClosed", err)
		}
	case <-time.After(5 * time.Second):
		t.Fatal("Accept() did not return after Close()")
	}
}

func TestRXEDialRefusedWithoutListener(t *testing.T) {
	ln := rxeListen(t, Config{})
	addr := ln.Addr().String()
	if err := ln.Close(); err != nil {
		t.Fatalf("Close() error: %v", err)
	}
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()
	start := time.Now()
	c, err := DialContext(ctx, "rdma", addr)
	if err == nil {
		c.Close()
		t.Fatalf("DialContext(%s) succeeded without a listener", addr)
	}
	var opErr *net.OpError
	if !errors.As(err, &opErr) || opErr.Op != "dial" || !errors.Is(err, syscall.ECONNREFUSED) {
		t.Fatalf("DialContext(%s) error = %v, want a dial *net.OpError with ECONNREFUSED", addr, err)
	}
	t.Logf("refused after %v: %v", time.Since(start).Round(time.Millisecond), err)
}

func TestRXEDialContextCancel(t *testing.T) {
	// A listener would not do: librdmacm completes the handshake before
	// Accept is ever called. The next address of the device's subnet has no
	// host behind it, so the address resolution retries until cancelled.
	dev := rdmatest.Device(t)
	nowhere := net.JoinHostPort(dev.IP.Next().String(), "4791")
	goroutines := runtime.NumGoroutine()
	ctx, cancel := context.WithCancel(context.Background())
	go func() {
		time.Sleep(200 * time.Millisecond)
		cancel()
	}()
	start := time.Now()
	c, err := DialContext(ctx, "rdma", nowhere)
	if err == nil {
		c.Close()
		t.Fatalf("DialContext(%s) succeeded without a host", nowhere)
	}
	if !errors.Is(err, context.Canceled) {
		t.Fatalf("DialContext(%s) error = %v, want context.Canceled", nowhere, err)
	}
	if elapsed := time.Since(start); elapsed > 3*time.Second {
		t.Errorf("DialContext() took %v to notice the cancel", elapsed)
	}
	deadline := time.Now().Add(5 * time.Second)
	for runtime.NumGoroutine() > goroutines && time.Now().Before(deadline) {
		time.Sleep(10 * time.Millisecond)
	}
	if after := runtime.NumGoroutine(); after > goroutines {
		t.Errorf("goroutines: %d before the dial, %d after its cancel", goroutines, after)
	}
}

func TestRXEIdleReadDeadline(t *testing.T) {
	client, _ := rxePair(t, Config{})
	const d = 50 * time.Millisecond
	client.SetReadDeadline(time.Now().Add(d))
	start := time.Now()
	n, err := client.Read(make([]byte, 1))
	elapsed := time.Since(start)
	if n != 0 || !isTimeout(err) || !errors.Is(err, os.ErrDeadlineExceeded) {
		t.Fatalf("Read() = %d, %v; want 0 and a deadline error", n, err)
	}
	if elapsed < d || elapsed > d+200*time.Millisecond {
		t.Errorf("Read() timed out after %v, want about %v", elapsed, d)
	}
}

func TestRXECloseWakesBlockedReadAndWrite(t *testing.T) {
	client, _ := rxePair(t, smallConfig)
	readErr := make(chan error, 1)
	go func() {
		_, err := client.Read(make([]byte, 1))
		readErr <- err
	}()
	writeErr := make(chan error, 1)
	go func() {
		// The peer never reads, so the buffers of both sides fill up.
		var err error
		for err == nil {
			_, err = client.Write(make([]byte, 64<<10))
		}
		writeErr <- err
	}()
	time.Sleep(500 * time.Millisecond)
	start := time.Now()
	if err := client.Close(); err != nil {
		t.Fatalf("Close() error: %v", err)
	}
	t.Logf("Close() with a healthy peer took %v", time.Since(start).Round(time.Millisecond))
	for name, ch := range map[string]chan error{"Read": readErr, "Write": writeErr} {
		select {
		case err := <-ch:
			if !errors.Is(err, net.ErrClosed) {
				t.Errorf("%s after Close() error = %v, want net.ErrClosed", name, err)
			}
		case <-time.After(5 * time.Second):
			t.Errorf("%s did not return after Close()", name)
		}
	}
}

func TestRXECloseWriteLetsThePeerWriteBack(t *testing.T) {
	client, server := rxePair(t, Config{})
	if _, err := client.Write([]byte("request")); err != nil {
		t.Fatalf("Write error: %v", err)
	}
	if err := client.CloseWrite(); err != nil {
		t.Fatalf("CloseWrite error: %v", err)
	}
	got, err := io.ReadAll(server)
	if err != nil || string(got) != "request" {
		t.Fatalf("ReadAll = %q, %v; want %q, nil", got, err, "request")
	}
	if _, err := client.Write([]byte("more")); !errors.Is(err, syscall.EPIPE) {
		t.Errorf("Write after CloseWrite error = %v, want EPIPE", err)
	}
	if _, err := server.Write([]byte("response")); err != nil {
		t.Fatalf("peer Write after EOF error: %v", err)
	}
	if err := server.Close(); err != nil {
		t.Fatalf("peer Close error: %v", err)
	}
	got, err = io.ReadAll(client)
	if err != nil || string(got) != "response" {
		t.Fatalf("ReadAll = %q, %v; want %q, nil", got, err, "response")
	}
}

func TestRXEDialerLocalAddrPinsTheSource(t *testing.T) {
	dev := rdmatest.Device(t)
	ln := rxeListen(t, Config{})
	acc := acceptAsync(ln)
	d := Dialer{LocalAddr: &Addr{IP: dev.IP.AsSlice()}, Timeout: rxeTimeout}
	client, err := d.Dial("rdma", ln.Addr().String())
	if err != nil {
		t.Fatalf("Dial() from %v error: %v", d.LocalAddr, err)
	}
	defer client.Close()
	r := <-acc
	if r.err != nil {
		t.Fatalf("AcceptRDMA() error: %v", r.err)
	}
	defer r.c.Close()
	local, ok := client.LocalAddr().(*Addr)
	if !ok || !local.IP.Equal(dev.IP.AsSlice()) || local.Port == 0 {
		t.Fatalf("client LocalAddr() = %v, want %v with a real port", client.LocalAddr(), dev.IP)
	}
	if r.c.RemoteAddr().String() != local.String() {
		t.Errorf("server RemoteAddr() = %v, want the client's LocalAddr() %v", r.c.RemoteAddr(), local)
	}
}

func threadCount(t *testing.T) int {
	t.Helper()
	status, err := os.ReadFile("/proc/self/status")
	if err != nil {
		t.Fatalf("read /proc/self/status: %v", err)
	}
	for _, line := range strings.Split(string(status), "\n") {
		if n, ok := strings.CutPrefix(line, "Threads:"); ok {
			c, err := strconv.Atoi(strings.TrimSpace(n))
			if err != nil {
				t.Fatalf("parse %q: %v", line, err)
			}
			return c
		}
	}
	t.Fatal("no Threads line in /proc/self/status")
	return 0
}

func cpuTime(t *testing.T) time.Duration {
	t.Helper()
	var ru syscall.Rusage
	if err := syscall.Getrusage(syscall.RUSAGE_SELF, &ru); err != nil {
		t.Fatalf("getrusage: %v", err)
	}
	return time.Duration(ru.Utime.Nano() + ru.Stime.Nano())
}

func TestRXEResourcesReturnToBaseline(t *testing.T) {
	rdmatest.Device(t)
	goroutines := runtime.NumGoroutine()
	ln := rxeListen(t, Config{})
	const n = 8
	for range n {
		acc := acceptAsync(ln)
		d := Dialer{Timeout: rxeTimeout}
		client, err := d.Dial("rdma", ln.Addr().String())
		if err != nil {
			t.Fatalf("Dial() error: %v", err)
		}
		r := <-acc
		if r.err != nil {
			t.Fatalf("AcceptRDMA() error: %v", r.err)
		}
		if _, err := client.Write([]byte("x")); err != nil {
			t.Fatalf("Write error: %v", err)
		}
		if _, err := r.c.Read(make([]byte, 1)); err != nil {
			t.Fatalf("Read error: %v", err)
		}
		if err := client.Close(); err != nil {
			t.Errorf("client Close() error: %v", err)
		}
		if err := r.c.Close(); err != nil {
			t.Errorf("server Close() error: %v", err)
		}
	}
	if err := ln.Close(); err != nil {
		t.Errorf("Listener Close() error: %v", err)
	}
	// With the last socket gone the poller exits too.
	deadline := time.Now().Add(5 * time.Second)
	for runtime.NumGoroutine() > goroutines && time.Now().Before(deadline) {
		time.Sleep(10 * time.Millisecond)
	}
	if after := runtime.NumGoroutine(); after > goroutines {
		t.Errorf("goroutines: %d before, %d after closing %d connections and the listener", goroutines, after, n)
	}
}

func TestRXEIdleConnectionsAreCheap(t *testing.T) {
	manyConnections(t)
	ln := rxeListen(t, smallConfig)
	const conns = 100
	var servers []*Conn
	var mu sync.Mutex
	var wg sync.WaitGroup
	wg.Add(1)
	go func() {
		defer wg.Done()
		for range conns {
			c, err := ln.AcceptRDMA()
			if err != nil {
				t.Errorf("AcceptRDMA() error: %v", err)
				return
			}
			mu.Lock()
			servers = append(servers, c)
			mu.Unlock()
		}
	}()
	d := Dialer{Timeout: rxeTimeout, Config: smallConfig}
	for i := range conns {
		c, err := d.Dial("rdma", ln.Addr().String())
		if err != nil {
			t.Fatalf("Dial() #%d error: %v", i, err)
		}
		defer c.Close()
	}
	wg.Wait()
	defer func() {
		for _, c := range servers {
			c.Close()
		}
	}()
	// Half of the servers wait in Read, as a server would, so that the poller
	// has descriptors to watch.
	for _, c := range servers[:conns/2] {
		go c.Read(make([]byte, 1))
	}
	time.Sleep(100 * time.Millisecond)
	threads, cpu := threadCount(t), cpuTime(t)
	time.Sleep(time.Second)
	threadsAfter, cpuAfter := threadCount(t), cpuTime(t)
	t.Logf("%d idle connections for 1s: CPU time +%v, threads %d -> %d", conns, cpuAfter-cpu, threads, threadsAfter)
	if cpuAfter-cpu >= 50*time.Millisecond {
		t.Errorf("CPU time grew by %v with %d idle connections, want < 50ms", cpuAfter-cpu, conns)
	}
	if threadsAfter-threads >= 10 {
		t.Errorf("threads grew from %d to %d with %d idle connections, want growth < 10", threads, threadsAfter, conns)
	}
}

// TestRXEBufferedDataPrecedesEOF checks that what a peer wrote before closing
// arrives in full ahead of io.EOF. The write stays within the half of the
// receive buffer a fresh rsocket advertises, since the peer reads nothing
// until the close.
func TestRXEBufferedDataPrecedesEOF(t *testing.T) {
	client, server := rxePair(t, Config{})
	want := make([]byte, 32<<10)
	newStream(7).rng.Read(want)
	if _, err := client.Write(want); err != nil {
		t.Fatalf("Write error: %v", err)
	}
	if err := client.Close(); err != nil {
		t.Fatalf("Close error: %v", err)
	}
	got, err := io.ReadAll(server)
	if err != nil {
		t.Fatalf("ReadAll error: %v", err)
	}
	if !bytes.Equal(got, want) {
		t.Fatalf("read %d bytes after the peer closed, want the %d it wrote", len(got), len(want))
	}
}
