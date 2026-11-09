//go:build linux && cgo

package rdma

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"io"
	"net"
	"net/netip"
	"os"
	"runtime"
	"strconv"
	"strings"
	"sync"
	"syscall"
	"testing"
	"time"

	"golang.org/x/net/nettest"

	"github.com/wzshiming/rdma/cm"
	"github.com/wzshiming/rdma/internal/rdmatest"
	"github.com/wzshiming/rdma/verbs"
)

const rxeTimeout = 20 * time.Second

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

// rxePair connects a client to a fresh listener on the device.
func rxePair(t testing.TB, cfg Config) (client, server *Conn) {
	t.Helper()
	ln := rxeListen(t, cfg)
	accepted := make(chan struct {
		c   *Conn
		err error
	}, 1)
	go func() {
		c, err := ln.AcceptRDMA()
		accepted <- struct {
			c   *Conn
			err error
		}{c, err}
	}()
	client = rxeDial(t, cfg, ln.Addr())
	r := <-accepted
	if r.err != nil {
		t.Fatalf("AcceptRDMA() error: %v", r.err)
	}
	t.Cleanup(func() { r.c.Close() })
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
	client, server := rxePair(t, Config{MessageSize: 1024, ReceiveDepth: 1, SendDepth: 1})
	go io.Copy(server, server)
	payload := 1024 - headerSize
	for _, n := range []int{1, 100, payload - 1, payload, payload + 1, 3*payload + 5, 100000} {
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
	for _, size := range []int{4 << 10, 64 << 10} {
		for _, depth := range []int{1, 64} {
			t.Run(fmt.Sprintf("%dKiB_depth%d", size>>10, depth), func(t *testing.T) {
				client, server := rxePair(t, Config{MessageSize: size, ReceiveDepth: depth, SendDepth: depth})
				const total = 1 << 30
				start := time.Now()
				transfer(t, client, server, total, 64<<10, client.CloseWrite)
				elapsed := time.Since(start)
				// Printed rather than logged so that the timing shows without -v.
				fmt.Printf("%s: %d MiB in %v (%.0f MiB/s)\n", t.Name(), total>>20, elapsed.Round(time.Millisecond), float64(total>>20)/elapsed.Seconds())
			})
		}
	}
}

func TestRXEConcurrentConnections(t *testing.T) {
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
			// Echo everything back, then close our side.
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
	if elapsed := time.Since(start); elapsed > 5*time.Second {
		t.Errorf("the refusal took %v, want it well within the 10s deadline", elapsed)
	}
}

func TestRXEDialContextCancel(t *testing.T) {
	ln := rxeListen(t, Config{}) // never accepts, so the request stays pending
	ctx, cancel := context.WithCancel(context.Background())
	go func() {
		time.Sleep(200 * time.Millisecond)
		cancel()
	}()
	start := time.Now()
	c, err := DialContext(ctx, "rdma", ln.Addr().String())
	if err == nil {
		c.Close()
		t.Fatal("DialContext() succeeded without an accept")
	}
	if !errors.Is(err, context.Canceled) {
		t.Fatalf("DialContext() error = %v, want context.Canceled", err)
	}
	if elapsed := time.Since(start); elapsed > 3*time.Second {
		t.Errorf("DialContext() took %v to notice the cancel", elapsed)
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
	client, _ := rxePair(t, Config{MessageSize: 256, ReceiveDepth: 1, SendDepth: 1})
	readErr := make(chan error, 1)
	go func() {
		_, err := client.Read(make([]byte, 1))
		readErr <- err
	}()
	writeErr := make(chan error, 1)
	go func() {
		_, err := client.Write(make([]byte, 4096)) // the peer never reads
		writeErr <- err
	}()
	time.Sleep(100 * time.Millisecond)
	if err := client.Close(); err != nil {
		t.Fatalf("Close() error: %v", err)
	}
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

// rawQP gives a cm ID the queue pair Connect needs.
func rawQP(t *testing.T, id *cm.ID) {
	t.Helper()
	pd, err := id.Device().AllocPD()
	if err != nil {
		t.Fatalf("AllocPD() error: %v", err)
	}
	cq, err := id.Device().CreateCQ(4, nil)
	if err != nil {
		t.Fatalf("CreateCQ() error: %v", err)
	}
	t.Cleanup(func() {
		id.Close()
		cq.Close()
		pd.Close()
	})
	attr := verbs.QPInitAttr{SendCQ: cq, RecvCQ: cq, Type: verbs.QPTypeRC, Cap: verbs.QPCap{MaxSendWR: 1, MaxRecvWR: 1, MaxSendSGE: 1, MaxRecvSGE: 1}}
	if _, err := id.CreateQP(pd, &attr); err != nil {
		t.Fatalf("CreateQP() error: %v", err)
	}
}

func TestRXEVersionMismatchIsRejected(t *testing.T) {
	dev := rdmatest.Device(t)
	t.Run("listener rejects", func(t *testing.T) {
		ln := rxeListen(t, Config{})
		go ln.Accept() // would return the next good connection; none comes
		ctx, cancel := context.WithTimeout(context.Background(), rxeTimeout)
		defer cancel()
		id, err := cm.Resolve(ctx, netip.AddrPortFrom(dev.IP, 0), ln.addr.addrPort())
		if err != nil {
			t.Fatalf("Resolve() error: %v", err)
		}
		rawQP(t, id)
		hello := encodeHello(Config{}.withDefaults())
		hello[4] = protocolVersion + 1
		_, err = id.Connect(ctx, &cm.ConnParam{PrivateData: hello})
		var rej *cm.RejectedError
		if !errors.As(err, &rej) || !bytes.Equal(rej.PrivateData[:rejectSize], encodeReject(rejectVersion)) {
			t.Fatalf("Connect() with version %d error = %v, want a *cm.RejectedError carrying %x", protocolVersion+1, err, encodeReject(rejectVersion))
		}
	})
	t.Run("dialer reports", func(t *testing.T) {
		raw, err := cm.Listen(netip.AddrPortFrom(dev.IP, 0), 1)
		if err != nil {
			t.Fatalf("cm.Listen() error: %v", err)
		}
		defer raw.Close()
		go func() {
			ctx, cancel := context.WithTimeout(context.Background(), rxeTimeout)
			defer cancel()
			req, err := raw.Accept(ctx)
			if err != nil {
				t.Errorf("cm Accept() error: %v", err)
				return
			}
			frame := encodeReject(rejectVersion)
			frame[4] = protocolVersion + 1
			req.Reject(frame)
		}()
		d := Dialer{Timeout: rxeTimeout}
		c, err := d.Dial("rdma", addrFromAddrPort(raw.Addr()).String())
		if err == nil {
			c.Close()
			t.Fatal("Dial() to a rejecting listener succeeded")
		}
		if !errors.Is(err, ErrProtocol) || !errors.Is(err, syscall.ECONNREFUSED) {
			t.Fatalf("Dial() error = %v, want ErrProtocol and ECONNREFUSED", err)
		}
		if !strings.Contains(err.Error(), "version 2") {
			t.Errorf("Dial() error = %q, want it to name the peer's version", err)
		}
	})
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
	ln := rxeListen(t, Config{})
	goroutines := runtime.NumGoroutine()
	registered := registeredBytes.Load()
	const n = 8
	for range n {
		accepted := make(chan *Conn, 1)
		go func() {
			c, err := ln.AcceptRDMA()
			if err != nil {
				t.Errorf("AcceptRDMA() error: %v", err)
			}
			accepted <- c
		}()
		d := Dialer{Timeout: rxeTimeout}
		client, err := d.Dial("rdma", ln.Addr().String())
		if err != nil {
			t.Fatalf("Dial() error: %v", err)
		}
		server := <-accepted
		if server == nil {
			t.FailNow()
		}
		if _, err := client.Write([]byte("x")); err != nil {
			t.Fatalf("Write error: %v", err)
		}
		if _, err := server.Read(make([]byte, 1)); err != nil {
			t.Fatalf("Read error: %v", err)
		}
		if err := client.Close(); err != nil {
			t.Errorf("client Close() error: %v", err)
		}
		if err := server.Close(); err != nil {
			t.Errorf("server Close() error: %v", err)
		}
	}
	if got := registeredBytes.Load(); got != registered {
		t.Errorf("registered memory: %d bytes before, %d after closing %d connections", registered, got, n)
	}
	deadline := time.Now().Add(5 * time.Second)
	for runtime.NumGoroutine() > goroutines && time.Now().Before(deadline) {
		time.Sleep(10 * time.Millisecond)
	}
	if after := runtime.NumGoroutine(); after > goroutines {
		t.Errorf("goroutines: %d before, %d after closing %d connections", goroutines, after, n)
	}
}

func TestRXEIdleConnectionsAreCheap(t *testing.T) {
	ln := rxeListen(t, Config{MessageSize: 4096, ReceiveDepth: 4, SendDepth: 4})
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
	d := Dialer{Timeout: rxeTimeout, Config: Config{MessageSize: 4096, ReceiveDepth: 4, SendDepth: 4}}
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
	threads, cpu := threadCount(t), cpuTime(t)
	time.Sleep(time.Second)
	threadsAfter, cpuAfter := threadCount(t), cpuTime(t)
	t.Logf("%d idle connections for 1s: CPU time +%v, threads %d -> %d", conns, cpuAfter-cpu, threads, threadsAfter)
	fmt.Printf("%s: %d idle connections for 1s: CPU time +%v, threads %d -> %d\n", t.Name(), conns, cpuAfter-cpu, threads, threadsAfter)
	if cpuAfter-cpu >= 50*time.Millisecond {
		t.Errorf("CPU time grew by %v with %d idle connections, want < 50ms", cpuAfter-cpu, conns)
	}
	if threadsAfter-threads >= 10 {
		t.Errorf("threads grew from %d to %d with %d idle connections, want growth < 10", threads, threadsAfter, conns)
	}
}
