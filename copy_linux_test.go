//go:build linux && !android && (amd64 || arm64 || loong64 || ppc64le || riscv64 || s390x)

package rdma

import (
	"io"
	"net"
	"sync"
	"testing"
	"time"

	"github.com/wzshiming/rdma/internal/rdmatest"
)

// TestRXECopy runs io.Copy both ways on each pair of engines with small
// buffers. ReadFrom and WriteTo move the bytes through the buffers of the
// native engine's streams, which they lend, and through buffers of their own
// over the librdmacm engine's, which lend nothing. librdmacm's rsend uses a
// grant before the write that placed it completes, which on rxe without FSRM
// now and then puts a message over the one before (see internal/rsproto):
// what a librdmacm end sends can arrive altered on such runners. That rxe's
// memcpy there stores each half of the 16-byte grant twice is inferred, not
// observed: from the kernel source and from the CPUs of the failed runs.
// Where a librdmacm end sends, the occasional "connection reset by peer",
// seen in 1 of 589 stress runs before the native engine waited for the
// completion of a grant's write and in 1 of 664 after, may come of the same
// flaw; that is unverified.
func TestRXECopy(t *testing.T) {
	forEachPair(t, func(t *testing.T, dial, listen engineName) {
		testRXECopy(t, smallConfig, dial, listen, 1<<30)
	})
}

// fullSource reads left bytes of the stream s, filling every buffer.
type fullSource struct {
	s    *testStream
	left int
}

func (r *fullSource) Read(p []byte) (int, error) {
	if r.left == 0 {
		return 0, io.EOF
	}
	n := min(len(p), r.left)
	r.s.produce(p[:n])
	r.left -= n
	return n, nil
}

// testRXECopy moves total bytes each way at once between a connection dialed
// with the engine dial and one a listener of the engine listen accepts, both
// with cfg otherwise, on the rdmatest device, with io.Copy, which enters
// ReadFrom on the sending ends and WriteTo on the receiving ones, and checks
// the hashes.
func testRXECopy(t *testing.T, cfg Config, dial, listen engineName, total int) {
	dev := rdmatest.Device(t)
	lc := ListenConfig{Config: cfg, eng: listen.engine()}
	ln, err := lc.Listen(t.Context(), "rdma", net.JoinHostPort(dev.IP.String(), "0"))
	if err != nil {
		t.Fatalf("Listen(%v:0) error: %v", dev.IP, err)
	}
	acc := acceptAsync(ln)
	d := Dialer{Config: cfg, Timeout: rxeTimeout, eng: dial.engine()}
	client, err := d.Dial("rdma", ln.Addr().String())
	if err != nil {
		ln.Close()
		t.Fatalf("Dial(%v) error: %v", ln.Addr(), err)
	}
	defer client.Close()
	r := <-acc
	ln.Close()
	if r.err != nil {
		t.Fatalf("AcceptRDMA() error: %v", r.err)
	}
	server := r.c
	defer server.Close()

	start := time.Now()
	var wg sync.WaitGroup
	var ins, outs []*testStream
	for i, ends := range [][2]*Conn{{client, server}, {server, client}} {
		in, out := newStream(byte(i)), newStream(byte(i))
		ins, outs = append(ins, in), append(outs, out)
		wg.Add(2)
		go func() {
			defer wg.Done()
			if _, err := io.Copy(ends[0], &fullSource{s: in, left: total}); err != nil {
				t.Errorf("io.Copy(conn, src) #%d error: %v", i, err)
			}
			if err := ends[0].CloseWrite(); err != nil {
				t.Errorf("CloseWrite #%d error: %v", i, err)
			}
		}()
		go func() {
			defer wg.Done()
			if _, err := io.Copy(&sink{s: out}, ends[1]); err != nil {
				t.Errorf("io.Copy(dst, conn) #%d error: %v", i, err)
			}
		}()
	}
	wg.Wait()
	elapsed := time.Since(start)
	t.Logf("%d MiB each way in %v (%.0f MiB/s each way)", total>>20, elapsed.Round(time.Millisecond), float64(total>>20)/elapsed.Seconds())
	from := [...]engineName{dial, listen}
	for i := range ins {
		if report := describeTransfer(ins[i], outs[i]); report != "" {
			t.Errorf("direction #%d, %s to %s: %s", i, from[i], from[1-i], report)
		}
	}
}
