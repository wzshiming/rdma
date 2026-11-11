package rdma

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"hash"
	"hash/crc32"
	"io"
	"math/rand/v2"
	"net"
	"os"
	"runtime"
	"sync"
	"testing"
	"time"

	"golang.org/x/net/nettest"
)

// listenFake starts a Listener on the fake network.
func listenFake(t testing.TB, nw *fakeNet, cfg Config) *Listener {
	t.Helper()
	lc := ListenConfig{Config: cfg, eng: librdmacmEngine{nw}}
	ln, err := lc.Listen(context.Background(), "rdma", "10.99.0.1:0")
	if err != nil {
		t.Fatalf("Listen() error: %v", err)
	}
	t.Cleanup(func() { ln.Close() })
	return ln
}

// dialFake dials addr on the fake network.
func dialFake(t testing.TB, nw *fakeNet, cfg Config, addr string) *Conn {
	t.Helper()
	d := Dialer{Config: cfg, eng: librdmacmEngine{nw}}
	c, err := d.Dial("rdma", addr)
	if err != nil {
		t.Fatalf("Dial(%s) error: %v", addr, err)
	}
	t.Cleanup(func() { c.Close() })
	return c
}

type accepted struct {
	c   *Conn
	err error
}

// acceptAsync runs ln.AcceptRDMA in the background.
func acceptAsync(ln *Listener) <-chan accepted {
	ch := make(chan accepted, 1)
	go func() {
		c, err := ln.AcceptRDMA()
		ch <- accepted{c, err}
	}()
	return ch
}

// fakePipe connects a dialer to a listener on the fake network, both with
// cfg, and returns the two ends.
func fakePipe(t testing.TB, nw *fakeNet, cfg Config) (client, server *Conn) {
	t.Helper()
	ln := listenFake(t, nw, cfg)
	acc := acceptAsync(ln)
	client = dialFake(t, nw, cfg, ln.Addr().String())
	r := <-acc
	if r.err != nil {
		t.Fatalf("AcceptRDMA() error: %v", r.err)
	}
	t.Cleanup(func() { r.c.Close() })
	return client, r.c
}

func TestConnOnFakeNetwork(t *testing.T) {
	configs := map[string]Config{
		"defaults": {},
		"tiny":     {SendBufferSize: 512, ReceiveBufferSize: 1024, SendQueueSize: 2, ReceiveQueueSize: 2},
		"large":    {SendBufferSize: 1 << 20, ReceiveBufferSize: 1 << 20, MaxInline: 256},
	}
	for name, cfg := range configs {
		t.Run(name, func(t *testing.T) {
			nettest.TestConn(t, func() (c1, c2 net.Conn, stop func(), err error) {
				client, server := fakePipe(t, newFakeNet(t), cfg)
				return client, server, func() {
					client.Close()
					server.Close()
				}, nil
			})
		})
	}
}

// testStream is a deterministic byte stream hashed as it is produced or
// consumed.
type testStream struct {
	rng  *rand.ChaCha8
	hash hash.Hash32
	n    int64
}

func newStream(seed byte) *testStream {
	var s [32]byte
	s[0] = seed
	return &testStream{rng: rand.NewChaCha8(s), hash: crc32.NewIEEE()}
}

// writeTo writes total bytes to w in chunks of at most chunk bytes.
func (s *testStream) writeTo(w io.Writer, total, chunk int) error {
	buf := make([]byte, chunk)
	for total > 0 {
		n := min(total, chunk)
		s.rng.Read(buf[:n])
		s.hash.Write(buf[:n])
		if _, err := w.Write(buf[:n]); err != nil {
			return err
		}
		s.n += int64(n)
		total -= n
	}
	return nil
}

// readFrom reads r until EOF in chunks of at most chunk bytes.
func (s *testStream) readFrom(r io.Reader, chunk int) error {
	buf := make([]byte, chunk)
	for {
		n, err := r.Read(buf)
		s.hash.Write(buf[:n])
		s.n += int64(n)
		if err == io.EOF {
			return nil
		}
		if err != nil {
			return err
		}
	}
}

func (s *testStream) sum() string { return fmt.Sprintf("%d bytes, crc %08x", s.n, s.hash.Sum32()) }

// transfer streams total bytes from w to r, closes w's sending side and
// checks the hashes.
func transfer(t testing.TB, w io.Writer, r io.Reader, total, chunk int, closeWrite func() error) {
	t.Helper()
	var wg sync.WaitGroup
	wg.Add(1)
	in := newStream(1)
	go func() {
		defer wg.Done()
		if err := in.writeTo(w, total, chunk); err != nil {
			t.Errorf("Write error: %v", err)
		}
		if err := closeWrite(); err != nil {
			t.Errorf("CloseWrite error: %v", err)
		}
	}()
	out := newStream(0)
	if err := out.readFrom(r, chunk); err != nil {
		t.Errorf("Read error: %v", err)
	}
	wg.Wait()
	if in.sum() != out.sum() {
		t.Errorf("sent %s, received %s", in.sum(), out.sum())
	}
}

func TestEchoRoundTrips(t *testing.T) {
	client, server := fakePipe(t, newFakeNet(t), Config{ReceiveBufferSize: 1024})
	go io.Copy(server, server)
	for _, n := range []int{1, 100, 1023, 1024, 1025, 3*1024 + 5, 100000} {
		want := make([]byte, n)
		newStream(byte(n)).rng.Read(want)
		// The echo holds at most a buffer, so the client reads while it
		// writes, as it would over TCP with a small window.
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

func TestTransferIsIntactUnderBackPressure(t *testing.T) {
	for name, cfg := range map[string]Config{
		"defaults": {},
		"small":    {SendBufferSize: 16 << 10, ReceiveBufferSize: 16 << 10, SendQueueSize: 8, ReceiveQueueSize: 8},
		"tiny":     {ReceiveBufferSize: 100},
	} {
		t.Run(name, func(t *testing.T) {
			client, server := fakePipe(t, newFakeNet(t), cfg)
			transfer(t, client, server, 16<<20, 64<<10, client.CloseWrite)
			// EOF is sticky and the other direction is intact.
			if n, err := server.Read(make([]byte, 1)); n != 0 || err != io.EOF {
				t.Errorf("Read after EOF = %d, %v; want 0, io.EOF", n, err)
			}
			transfer(t, server, client, 1<<20, 3000, server.CloseWrite)
		})
	}
}

func TestConcurrentConnectionsOnOneListener(t *testing.T) {
	nw := newFakeNet(t)
	ln := listenFake(t, nw, Config{ReceiveBufferSize: 4096})
	const conns = 32
	const total = 1 << 20
	var wg sync.WaitGroup
	for range conns {
		wg.Add(1)
		go func() {
			defer wg.Done()
			server, err := ln.AcceptRDMA()
			if err != nil {
				t.Errorf("AcceptRDMA() error: %v", err)
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
			d := Dialer{eng: librdmacmEngine{nw}}
			client, err := d.Dial("rdma", ln.Addr().String())
			if err != nil {
				t.Errorf("Dial() #%d error: %v", i, err)
				ln.Close()
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

func TestCloseWriteLetsThePeerWriteBack(t *testing.T) {
	a, b := fakePipe(t, newFakeNet(t), Config{})
	if _, err := a.Write([]byte("request")); err != nil {
		t.Fatalf("Write error: %v", err)
	}
	if err := a.CloseWrite(); err != nil {
		t.Fatalf("CloseWrite error: %v", err)
	}
	got, err := io.ReadAll(b)
	if err != nil || string(got) != "request" {
		t.Fatalf("ReadAll = %q, %v; want %q, nil", got, err, "request")
	}
	_, err = a.Write([]byte("more"))
	var opErr *net.OpError
	if !errors.As(err, &opErr) || opErr.Op != "write" || !errors.Is(err, errPipe) {
		t.Errorf("Write after CloseWrite error = %v, want a write *net.OpError with EPIPE", err)
	}
	if err := a.CloseWrite(); err != nil {
		t.Errorf("second CloseWrite error = %v, want nil", err)
	}
	if _, err := b.Write([]byte("response")); err != nil {
		t.Fatalf("peer Write after reading EOF error: %v", err)
	}
	if err := b.Close(); err != nil {
		t.Fatalf("peer Close error: %v", err)
	}
	got, err = io.ReadAll(a)
	if err != nil || string(got) != "response" {
		t.Fatalf("ReadAll = %q, %v; want %q, nil", got, err, "response")
	}
}

func isTimeout(err error) bool {
	var nerr net.Error
	return errors.As(err, &nerr) && nerr.Timeout()
}

func TestIdleReadDeadlineTimesOutWithinTolerance(t *testing.T) {
	a, _ := fakePipe(t, newFakeNet(t), Config{})
	const d = 50 * time.Millisecond
	start := time.Now()
	a.SetReadDeadline(start.Add(d))
	n, err := a.Read(make([]byte, 1))
	elapsed := time.Since(start)
	if n != 0 || !isTimeout(err) || !errors.Is(err, os.ErrDeadlineExceeded) {
		t.Fatalf("Read() = %d, %v; want 0 and a deadline error", n, err)
	}
	if elapsed < d || elapsed > d+200*time.Millisecond {
		t.Errorf("Read() timed out after %v, want about %v", elapsed, d)
	}
}

func TestCloseWakesBlockedReadAndWrite(t *testing.T) {
	a, _ := fakePipe(t, newFakeNet(t), Config{ReceiveBufferSize: 1024})
	readErr := make(chan error, 1)
	go func() {
		_, err := a.Read(make([]byte, 1))
		readErr <- err
	}()
	writeErr := make(chan error, 1)
	go func() {
		_, err := a.Write(make([]byte, 4096)) // the peer never reads
		writeErr <- err
	}()
	time.Sleep(50 * time.Millisecond)
	if err := a.Close(); err != nil {
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

func TestResourcesReturnToBaseline(t *testing.T) {
	nw := newFakeNet(t)
	goroutines := runtime.NumGoroutine()
	ln := listenFake(t, nw, Config{})
	const n = 16
	for range n {
		acc := acceptAsync(ln)
		d := Dialer{eng: librdmacmEngine{nw}}
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
	if open := nw.openSockets(); open != 0 {
		t.Errorf("%d sockets still open after closing %d connections and the listener", open, n)
	}
	if attached := nw.attachedSockets(); attached != 0 {
		t.Errorf("%d sockets still attached to the poller", attached)
	}
	deadline := time.Now().Add(5 * time.Second)
	for runtime.NumGoroutine() > goroutines && time.Now().Before(deadline) {
		time.Sleep(10 * time.Millisecond)
	}
	if after := runtime.NumGoroutine(); after > goroutines {
		t.Errorf("goroutines: %d before, %d after closing %d connections", goroutines, after, n)
	}
}
