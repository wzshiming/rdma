package rdma

import (
	"bytes"
	"errors"
	"fmt"
	"hash"
	"hash/crc32"
	"io"
	"math/rand/v2"
	"net"
	"sync"
	"testing"
	"time"
)

// stream is a deterministic byte stream hashed as it is produced or consumed.
type stream struct {
	rng  *rand.ChaCha8
	hash hash.Hash32
	n    int64
}

func newStream(seed byte) *stream {
	var s [32]byte
	s[0] = seed
	return &stream{rng: rand.NewChaCha8(s), hash: crc32.NewIEEE()}
}

// writeTo writes total bytes to w in chunks of at most chunk bytes.
func (s *stream) writeTo(w io.Writer, total, chunk int) error {
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
func (s *stream) readFrom(r io.Reader, chunk int) error {
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

func (s *stream) sum() string { return fmt.Sprintf("%d bytes, crc %08x", s.n, s.hash.Sum32()) }

// transfer streams total bytes from w to r and checks the hashes.
func transfer(t *testing.T, w io.Writer, r io.Reader, total, chunk int, closeWrite func() error) {
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

// fullDuplex streams total bytes in both directions at once, after skipping
// skip bytes already on each stream.
func fullDuplex(t *testing.T, a, b *Conn, total, chunk, skip int) {
	t.Helper()
	var wg sync.WaitGroup
	wg.Add(2)
	go func() {
		defer wg.Done()
		transfer(t, a, &skipReader{b, skip}, total, chunk, a.CloseWrite)
	}()
	go func() {
		defer wg.Done()
		transfer(t, b, &skipReader{a, skip}, total, chunk, b.CloseWrite)
	}()
	wg.Wait()
}

// skipReader discards the first skip bytes of r.
type skipReader struct {
	r    io.Reader
	skip int
}

func (s *skipReader) Read(p []byte) (int, error) {
	for s.skip > 0 {
		n, err := s.r.Read(p[:min(len(p), s.skip)])
		s.skip -= n
		if err != nil {
			return 0, err
		}
	}
	return s.r.Read(p)
}

func TestFullDuplexAtDepthOneAndSixtyFour(t *testing.T) {
	for _, depth := range []int{1, 64} {
		t.Run(fmt.Sprintf("depth%d", depth), func(t *testing.T) {
			f := newFakeFabric(t)
			a, b := fakePipe(t, f, Config{MessageSize: 4096, ReceiveDepth: depth, SendDepth: depth})
			defer a.Close()
			defer b.Close()
			start := time.Now()
			fullDuplex(t, a, b, 64<<20, 64<<10, 0)
			t.Logf("64 MiB each way in %v", time.Since(start))
		})
	}
}

func TestWriterOnlyReaderOnlyAtDepthOne(t *testing.T) {
	f := newFakeFabric(t)
	a, b := fakePipe(t, f, Config{MessageSize: 1024, ReceiveDepth: 1, SendDepth: 1})
	defer a.Close()
	defer b.Close()
	transfer(t, a, b, 8<<20, 3000, a.CloseWrite)
}

func TestEchoStraddlesMessageBoundariesAtDepthOne(t *testing.T) {
	const msg = 1024
	f := newFakeFabric(t)
	a, b := fakePipe(t, f, Config{MessageSize: msg, ReceiveDepth: 1, SendDepth: 1})
	defer a.Close()
	defer b.Close()
	go io.Copy(b, b)
	payload := msg - headerSize
	for _, n := range []int{1, payload - 1, payload, payload + 1, 2*payload - 1, 2 * payload, 2*payload + 1, 7*payload + 13} {
		want := make([]byte, n)
		newStream(byte(n)).rng.Read(want)
		// The echo holds at most one message, so the client must read
		// while it writes, as it would over TCP with a tiny window.
		werr := make(chan error, 1)
		go func() {
			_, err := a.Write(want)
			werr <- err
		}()
		got := make([]byte, n)
		if _, err := io.ReadFull(a, got); err != nil {
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

func TestEOFAfterAlignedAndUnalignedTotals(t *testing.T) {
	const msg = 512
	payload := msg - headerSize
	for _, total := range []int{0, 1, payload, payload + 1, 5 * payload, 5*payload - 1, 5*payload + 1} {
		t.Run(fmt.Sprint(total), func(t *testing.T) {
			f := newFakeFabric(t)
			a, b := fakePipe(t, f, Config{MessageSize: msg, ReceiveDepth: 2, SendDepth: 2})
			defer a.Close()
			defer b.Close()
			transfer(t, a, b, total, payload, a.CloseWrite)
			// EOF is sticky and the connection is otherwise intact.
			if n, err := b.Read(make([]byte, 1)); n != 0 || err != io.EOF {
				t.Errorf("Read after EOF = %d, %v; want 0, io.EOF", n, err)
			}
		})
	}
}

func TestCloseWriteLetsThePeerWriteBack(t *testing.T) {
	f := newFakeFabric(t)
	a, b := fakePipe(t, f, Config{})
	defer a.Close()
	defer b.Close()
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
	if _, err := a.Write([]byte("more")); err == nil {
		t.Error("Write after CloseWrite succeeded")
	} else {
		var opErr *net.OpError
		if !errors.As(err, &opErr) || opErr.Op != "write" || !errors.Is(err, errPipe) {
			t.Errorf("Write after CloseWrite error = %v, want a write *net.OpError with EPIPE", err)
		}
	}
	if err := a.CloseWrite(); err != nil {
		t.Errorf("second CloseWrite error = %v, want nil", err)
	}
	if _, err := b.Write([]byte("response")); err != nil {
		t.Fatalf("peer Write after reading EOF error: %v", err)
	}
	if err := b.CloseWrite(); err != nil {
		t.Fatalf("peer CloseWrite error: %v", err)
	}
	got, err = io.ReadAll(a)
	if err != nil || string(got) != "response" {
		t.Fatalf("ReadAll = %q, %v; want %q, nil", got, err, "response")
	}
}

func TestBothSidesOutOfCreditsStillMakeProgress(t *testing.T) {
	const msg = 256
	f := newFakeFabric(t)
	a, b := fakePipe(t, f, Config{MessageSize: msg, ReceiveDepth: 1, SendDepth: 1})
	defer a.Close()
	defer b.Close()
	// Each side spends its single credit without the other reading.
	one := make([]byte, msg-headerSize)
	for _, c := range []*Conn{a, b} {
		if _, err := c.Write(one); err != nil {
			t.Fatalf("Write error: %v", err)
		}
	}
	a.mu.Lock()
	b.mu.Lock()
	if a.dataCredits != 0 || b.dataCredits != 0 {
		t.Fatalf("credits after one message each = %d, %d; want 0, 0", a.dataCredits, b.dataCredits)
	}
	a.mu.Unlock()
	b.mu.Unlock()
	// Both now stream to each other while reading: every interleaving of
	// grants must come through.
	fullDuplex(t, a, b, 256<<10, 1000, len(one))
}
