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
	"slices"
	"strings"
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
// consumed. produce and consume note where each chunk starts, and consume
// checks the bytes against those the seed produces.
type testStream struct {
	seed byte
	rng  *rand.ChaCha8
	hash hash.Hash32
	n    int64

	starts []int64 // the offsets the chunks of produce or consume start at
	want   []byte  // consume's room for the bytes the seed produces
	diff   divergence
}

func newStream(seed byte) *testStream {
	var s [32]byte
	s[0] = seed
	return &testStream{seed: seed, rng: rand.NewChaCha8(s), hash: crc32.NewIEEE()}
}

// produce fills p with the next bytes of the stream.
func (s *testStream) produce(p []byte) {
	s.starts = append(s.starts, s.n)
	s.rng.Read(p)
	s.hash.Write(p)
	s.n += int64(len(p))
}

// consume takes p, the next bytes received, and checks them against the next
// bytes of the stream.
func (s *testStream) consume(p []byte) {
	s.starts = append(s.starts, s.n)
	s.hash.Write(p)
	if cap(s.want) < len(p) {
		s.want = make([]byte, len(p))
	}
	want := s.want[:len(p)]
	s.rng.Read(want)
	if !bytes.Equal(p, want) {
		s.diff.note(s.n, want, p)
	}
	s.diff.sample(s.n, want, p)
	s.n += int64(len(p))
}

// divergence is where the bytes a stream consumed depart from those its seed
// produces: runs of bytes that differ, set apart by runGap bytes that agree or
// more, as random bytes agree by chance one time in 256.
type divergence struct {
	runs        int
	bytes       int64  // that differ, in all
	first, last int64  // the offsets of the first and the last that differ
	firstEnd    int64  // one past the last byte of the first run
	want, got   []byte // up to sampleSize bytes of each side from first
}

const (
	runGap     = 32
	sampleSize = 32
)

// note counts the bytes of got, consumed at off, that differ from want.
func (d *divergence) note(off int64, want, got []byte) {
	for i := range got {
		if got[i] == want[i] {
			continue
		}
		at := off + int64(i)
		if d.runs == 0 {
			d.first = at
		}
		if d.runs == 0 || at-d.last > runGap {
			d.runs++
		}
		if d.runs == 1 {
			d.firstEnd = at + 1
		}
		d.bytes++
		d.last = at
	}
}

// sample keeps the bytes of got and want, consumed at off, from the first that
// differs on until it holds sampleSize of each.
func (d *divergence) sample(off int64, want, got []byte) {
	if d.runs == 0 || len(d.got) == sampleSize {
		return
	}
	i := int(max(d.first-off, 0))
	n := min(sampleSize-len(d.got), len(got)-i)
	d.want = append(d.want, want[i:i+n]...)
	d.got = append(d.got, got[i:i+n]...)
}

// How far from a departure describeTransfer looks for where the bytes
// received there were sent, and how many chunks it shows on each side.
const (
	searchWindow = 1 << 20
	nearbyChunks = 3
)

// describeTransfer returns what tells the bytes out consumed from those in
// produced, empty when they are the same: where they first depart and how, or
// that the hashes alone disagree.
func describeTransfer(in, out *testStream) string {
	d := &out.diff
	if d.runs == 0 && in.sum() == out.sum() {
		return ""
	}
	var b strings.Builder
	fmt.Fprintf(&b, "sent %s, received %s", in.sum(), out.sum())
	if d.runs == 0 {
		b.WriteString("; every byte received is as sent")
		return b.String()
	}
	fmt.Fprintf(&b, "\nthe bytes received depart from those sent at offset %d (%#x) in a run of %d bytes; %d bytes differ in %d runs, the last at offset %d",
		d.first, d.first, d.firstEnd-d.first, d.bytes, d.runs, d.last)
	fmt.Fprintf(&b, "\nsent:     % x\nreceived: % x\n%s", d.want, d.got, in.whereSent(d.first, d.got))
	for _, side := range []struct {
		name string
		s    *testStream
	}{{"sent", in}, {"received", out}} {
		if starts := side.s.startsNear(d.first); starts != "" {
			fmt.Fprintf(&b, "\nchunks %s start at %s from there", side.name, starts)
		}
	}
	return b.String()
}

// whereSent tells where, within searchWindow bytes of off, the stream holds
// the first 16 bytes of got.
func (s *testStream) whereSent(off int64, got []byte) string {
	lo, hi := max(off-searchWindow, 0), min(off+searchWindow, s.n)
	if hi > lo {
		rng := newStream(s.seed).rng
		skip := make([]byte, 64<<10)
		for left := lo; left > 0; left -= int64(len(skip)) {
			rng.Read(skip[:min(left, int64(len(skip)))])
		}
		window := make([]byte, hi-lo)
		rng.Read(window)
		if i := bytes.Index(window, got[:min(len(got), 16)]); i >= 0 {
			at := lo + int64(i)
			return fmt.Sprintf("the bytes received there were sent at offset %d (%+d)", at, at-off)
		}
	}
	return fmt.Sprintf("the bytes received there were not sent within %d bytes of it", searchWindow)
}

// startsNear returns where the chunk holding off and nearbyChunks chunks on
// each side of it start, as offsets from off, empty when no chunk holds it.
func (s *testStream) startsNear(off int64) string {
	k, found := slices.BinarySearch(s.starts, off)
	if !found {
		k--
	}
	if k < 0 {
		return ""
	}
	var starts []string
	for _, start := range s.starts[max(k-nearbyChunks, 0):min(k+nearbyChunks+1, len(s.starts))] {
		starts = append(starts, fmt.Sprintf("%+d", start-off))
	}
	return strings.Join(starts, ", ")
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

// A stream received intact has nothing to report.
func TestTransferReportIsEmptyForAnIntactStream(t *testing.T) {
	in, out := newStream(9), newStream(9)
	b := make([]byte, 64<<10)
	in.produce(b)
	out.consume(b[:5000])
	out.consume(b[5000:])
	if report := describeTransfer(in, out); report != "" {
		t.Errorf("describeTransfer() = %q, want empty", report)
	}
}

// A stream whose bytes depart from those sent says where they first depart,
// how long that run is, what each side has there, where the bytes it got
// were sent, and where the chunks of each side start nearby.
func TestTransferReportShowsWhereTheBytesDepart(t *testing.T) {
	const total, sentChunk, receivedChunk = 256 << 10, 32 << 10, 3000
	const at, size, lap = 100000, 5000, 32 << 10
	in, out := newStream(9), newStream(9)
	sent := make([]byte, total)
	for off := 0; off < total; off += sentChunk {
		in.produce(sent[off : off+sentChunk])
	}
	// A stretch arrives as it was sent a lap of the buffer earlier.
	got := bytes.Clone(sent)
	copy(got[at:at+size], sent[at-lap:])
	if got[at] == sent[at] || got[at+size-1] == sent[at+size-1] {
		t.Fatal("the stretch replaced begins or ends with a byte that agrees; pick another")
	}
	for off := 0; off < total; off += receivedChunk {
		out.consume(got[off:min(off+receivedChunk, total)])
	}
	report := describeTransfer(in, out)
	for _, want := range []string{
		fmt.Sprintf("sent %s, received %s", in.sum(), out.sum()),
		fmt.Sprintf("the bytes received depart from those sent at offset %d (%#x) in a run of %d bytes", at, at, size),
		fmt.Sprintf("sent:     % x", sent[at:at+sampleSize]),
		fmt.Sprintf("received: % x", got[at:at+sampleSize]),
		fmt.Sprintf("the bytes received there were sent at offset %d (%+d)", at-lap, -lap),
		"chunks sent start at -100000, -67232, -34464, -1696, +31072, +63840, +96608 from there",
		"chunks received start at -10000, -7000, -4000, -1000, +2000, +5000, +8000 from there",
	} {
		if !strings.Contains(report, want) {
			t.Errorf("report lacks %q:\n%s", want, report)
		}
	}
}

// A stream whose bytes all arrived as sent while its hash disagrees says so.
func TestTransferReportTellsAHashAloneDisagrees(t *testing.T) {
	in, out := newStream(9), newStream(9)
	b := make([]byte, 4096)
	in.produce(b)
	out.consume(b)
	out.hash.Write([]byte{0})
	want := fmt.Sprintf("sent %s, received %s; every byte received is as sent", in.sum(), out.sum())
	if report := describeTransfer(in, out); report != want {
		t.Errorf("describeTransfer() = %q, want %q", report, want)
	}
}

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
