//go:build linux && !android && (amd64 || arm64 || loong64 || ppc64le || riscv64 || s390x)

package rdma

import (
	"io"
	"strconv"
	"testing"
	"time"

	"github.com/wzshiming/rdma/internal/rdmatest"
	"github.com/wzshiming/rdma/internal/rsproto"
)

// largeBuffers gives a connection 1 MiB buffers, which rsockets double, so
// that its peer may write it 1 MiB, half its receive buffer, a grant.
var largeBuffers = Config{SendBufferSize: 1 << 20, ReceiveBufferSize: 1 << 20}

// largestRead reads r into the buffers it is handed and keeps the most bytes
// a read returned: under ReadFrom on the native engine, which sends what
// each read returns as a message of its own, the largest message sent.
type largestRead struct {
	r       io.Reader
	largest int
}

func (l *largestRead) Read(p []byte) (int, error) {
	n, err := l.r.Read(p)
	l.largest = max(l.largest, n)
	return n, err
}

// A native sender with 1 MiB buffers sends messages larger than the 64 KiB
// stock rsockets cuts its sends at, RS_MAX_TRANSFER, and a receiver of either
// engine with buffers as large takes them intact.
func TestRXENativeSendsMessagesAboveMaxTransfer(t *testing.T) {
	rdmatest.Device(t)
	for _, listen := range engines {
		t.Run("to "+string(listen), func(t *testing.T) {
			client, server := rxeEnginePair(t, largeBuffers, engineNative, listen)
			in, out := newStream(1), newStream(1)
			received := async(func() (int64, error) { return io.Copy(&sink{s: out}, server) })
			src := &largestRead{r: &fullSource{s: in, left: 8 << 20}}
			if _, err := client.ReadFrom(src); err != nil {
				t.Fatalf("ReadFrom error: %v", err)
			}
			if err := client.CloseWrite(); err != nil {
				t.Fatalf("CloseWrite error: %v", err)
			}
			if r := <-received; r.err != nil {
				t.Fatalf("io.Copy(dst, conn) error: %v", r.err)
			}
			if in.sum() != out.sum() {
				t.Errorf("sent %s, received %s", in.sum(), out.sum())
			}
			if src.largest <= rsproto.MaxTransfer {
				t.Errorf("the largest message sent was %d bytes, want more than RS_MAX_TRANSFER, %d", src.largest, rsproto.MaxTransfer)
			}
		})
	}
}

// rstreamBytes is the byte stream rstream sends and checks under -T v, with
// format_buf and verify_buf of librdmacm's examples: byte k is k mod 256.
type rstreamBytes struct{ next byte }

func (r *rstreamBytes) Read(p []byte) (int, error) {
	for i := range p {
		p[i] = r.next
		r.next++
	}
	return len(p), nil
}

// rstream's client, which checks what it reads (-T v), takes intact the
// messages larger than RS_MAX_TRANSFER that a native listener with 1 MiB
// buffers sends back in an exchange of 1 MiB transfers with buffers as large.
func TestRXERstreamTakesMessagesAboveMaxTransfer(t *testing.T) {
	rstream := rstreamPath(t)
	cfg := largeBuffers
	ln := rxeListen(t, cfg)
	addr := ln.Addr().(*Addr)
	const iterations, count, size = 2, 2, 1 << 20
	args := append(rstreamCustomArgs("-s", addr.IP, addr.Port, iterations, count, size), "-T", "b", "-T", "v", "-B", strconv.Itoa(cfg.SendBufferSize))
	wait := startRstream(t, rstream, args...)
	var conn *Conn
	select {
	case r := <-acceptAsync(ln):
		if r.err != nil {
			t.Fatalf("AcceptRDMA() error: %v", r.err)
		}
		conn = r.c
	case <-time.After(rstreamTimeout):
		t.Fatal("rstream did not connect")
	}
	defer conn.Close()
	conn.SetDeadline(time.Now().Add(rstreamTimeout))
	src := &largestRead{r: new(rstreamBytes)}
	buf := make([]byte, count*size)
	// Each turn of the exchange reads what rstream sends and sends as much back.
	exchange := func(n int) {
		t.Helper()
		if _, err := io.ReadFull(conn, buf[:n]); err != nil {
			t.Fatalf("read of %d bytes: %v", n, err)
		}
		if _, err := conn.ReadFrom(io.LimitReader(src, int64(n))); err != nil {
			t.Fatalf("ReadFrom of %d bytes: %v", n, err)
		}
	}
	exchange(rstreamSync)
	for range iterations {
		exchange(count * size)
	}
	if n, err := io.Copy(io.Discard, conn); err != nil || n != 0 {
		t.Errorf("after the exchange: read %d bytes, %v; want 0 and EOF", n, err)
	}
	wait()
	if src.largest <= rsproto.MaxTransfer {
		t.Errorf("the largest message sent was %d bytes, want more than RS_MAX_TRANSFER, %d", src.largest, rsproto.MaxTransfer)
	}
}
