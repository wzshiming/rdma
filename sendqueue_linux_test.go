//go:build linux && !android && (amd64 || arm64 || loong64 || ppc64le || riscv64 || s390x)

package rdma

import (
	"bytes"
	"io"
	"os"
	"strings"
	"syscall"
	"testing"
	"time"

	"github.com/wzshiming/rdma/internal/rdmatest"
	"github.com/wzshiming/rdma/internal/rsproto"
)

// sendQueueProbe looks into c, a connection of the native engine, for the
// tests of its send completion queue.
type sendQueueProbe struct{ c *Conn }

// armed returns the kinds of completions whose queues the transport has
// armed.
func (p sendQueueProbe) armed() interest {
	proc := &qpTransportOf(p.c).proc
	proc.mu.Lock()
	defer proc.mu.Unlock()
	return proc.armed
}

// sendsDone reports whether the stream has been fed the completion of every
// send it posted, without processing anything for it.
func (p sendQueueProbe) sendsDone() bool {
	s := p.c.s.(*nativeStream)
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.sendsDone()
}

// spoilGrants spoils the keys of the grants the peer gave, so that the peer
// refuses the next send of data with a remote access error.
func (p sendQueueProbe) spoilGrants() {
	s := p.c.s.(*nativeStream)
	s.mu.Lock()
	defer s.mu.Unlock()
	table := qpTransportOf(p.c).mem.GrantTable.Buf
	order := rsproto.ConnData{BigEndian: bigEndian}.ByteOrder()
	for i := range rsproto.SGLSize {
		b := table[i*rsproto.SGESize:]
		g := rsproto.GetSGE(b, order)
		g.Key = ^g.Key
		rsproto.PutSGE(b, order, g)
	}
}

// While a connection of the native engine waits only for something to
// arrive, the completion of its own send wakes nothing: it waits on the send
// queue and goes to the stream with what arrives.
func TestRXESendQueueLeftAloneByAReader(t *testing.T) {
	rdmatest.Device(t)
	client, server := rxeEnginePair(t, Config{}, EngineNative, EngineNative)
	probe := sendQueueProbe{client}
	buf := make([]byte, 1)
	read := async(func() (int64, error) {
		n, err := client.Read(buf)
		return int64(n), err
	})
	eventually(t, "the Read waiting for arrivals", func() bool { return probe.armed()&arrivals != 0 })
	armed := probe.armed()
	if _, err := client.Write([]byte("x")); err != nil {
		t.Fatalf("Write error: %v", err)
	}
	if _, err := io.ReadFull(server, make([]byte, 1)); err != nil {
		t.Fatalf("peer ReadFull error: %v", err)
	}
	// Ample time for the acknowledgement of the peer's device to complete
	// the send.
	time.Sleep(100 * time.Millisecond)
	if probe.sendsDone() {
		t.Errorf("the completion of a send was taken while the connection waited only for arrivals, armed for %v", armed)
	}
	if _, err := server.Write([]byte("y")); err != nil {
		t.Fatalf("peer Write error: %v", err)
	}
	if r := await(t, "the Read waiting for arrivals", read); r.n != 1 || r.err != nil || buf[0] != 'y' {
		t.Fatalf("the Read waiting for arrivals = %d, %v with %q; want 1 and nil with %q", r.n, r.err, buf[:r.n], "y")
	}
	if !probe.sendsDone() {
		t.Error("the completion of the send was not taken with the arrival")
	}
}

// A connection of the native engine whose Writes wait for room in its send
// buffer while the peer reads nothing and sends nothing is woken by the
// completions of its sends alone.
func TestRXESendQueueWakesAWriterWithThePeerSilent(t *testing.T) {
	rdmatest.Device(t)
	// Each Write fills the 32 KiB send buffer; the peer granted all of its
	// 2 MiB receive buffer as the connection came up.
	const total, chunk = 1 << 20, 32 << 10
	client, server := rxeEnginePair(t, Config{SendBufferSize: chunk / 2, ReceiveBufferSize: total}, EngineNative, EngineNative)
	in := newStream(29)
	client.SetWriteDeadline(time.Now().Add(5 * time.Second))
	if err := in.writeTo(client, total, chunk); err != nil {
		t.Fatalf("Write after %d bytes with the peer silent error: %v", in.n, err)
	}
	server.SetReadDeadline(time.Now().Add(rxeTimeout))
	out := newStream(0)
	if err := out.readFrom(io.LimitReader(server, total), chunk); err != nil {
		t.Fatalf("peer Read after %d bytes error: %v", out.n, err)
	}
	if in.sum() != out.sum() {
		t.Errorf("wrote %s, the peer read %s", in.sum(), out.sum())
	}
}

// When the peer closes a connection of the native engine that is not
// reading, what the peer sent, its end of the connection included, and the
// completion of the connection's own send wait on their queues as the link
// goes down. The stream takes what both queues hold before the loss of the
// link, so it reads the data and io.EOF, not ECONNRESET.
func TestRXESendQueueDrainedBeforeTheLoss(t *testing.T) {
	rdmatest.Device(t)
	client, server := rxeEnginePair(t, Config{}, EngineNative, EngineNative)
	if _, err := client.Write([]byte("ping")); err != nil {
		t.Fatalf("Write error: %v", err)
	}
	if _, err := io.ReadFull(server, make([]byte, 4)); err != nil {
		t.Fatalf("peer ReadFull error: %v", err)
	}
	want := make([]byte, 4<<10)
	newStream(31).rng.Read(want)
	if _, err := server.Write(want); err != nil {
		t.Fatalf("peer Write error: %v", err)
	}
	if err := server.Close(); err != nil {
		t.Fatalf("peer Close error: %v", err)
	}
	lossCtx := qpTransportOf(client).lossCtx
	eventually(t, "the loss of the link", func() bool { return lossCtx.Err() != nil })
	client.SetReadDeadline(time.Now().Add(5 * time.Second))
	got, err := io.ReadAll(client)
	if !bytes.Equal(got, want) || err != nil {
		t.Errorf("read %d bytes and %v once the peer closed and the link went down; want its %d bytes and io.EOF", len(got), err, len(want))
	}
}

// A send of a connection of the native engine that the peer refuses wakes a
// Read waiting for arrivals with ECONNRESET, though nothing armed the send
// queue it fails on: the queue pair goes to the error state, and the
// receives it flushes raise the event of the receive queue.
func TestRXESendQueueFailureWakesAReader(t *testing.T) {
	rdmatest.Device(t)
	client, _ := rxeEnginePair(t, Config{}, EngineNative, EngineNative)
	probe := sendQueueProbe{client}
	read := async(func() (int64, error) {
		n, err := client.Read(make([]byte, 1))
		return int64(n), err
	})
	eventually(t, "the Read waiting for arrivals", func() bool { return probe.armed()&arrivals != 0 })
	armed := probe.armed()
	probe.spoilGrants()
	if _, err := client.Write([]byte("x")); err != nil {
		t.Fatalf("Write error: %v", err)
	}
	r := await(t, "the Read waiting as the send failed", read)
	t.Logf("armed for %v as the send failed; the connection manager reported the end: %v", armed, qpTransportOf(client).lossCtx.Err() != nil)
	if r.n != 0 || !isOpError(r.err, "read", syscall.ECONNRESET) {
		t.Errorf("the Read waiting as the send failed = %d, %v; want 0 and a read *net.OpError with ECONNRESET", r.n, r.err)
	}
}

// A connection of the native engine closed while the completions of its
// sends wait on their queue, nothing having taken them, returns within
// closeTimeout, and the peer reads what was written and io.EOF.
func TestRXESendQueueReapedByClose(t *testing.T) {
	rdmatest.Device(t)
	client, server := rxeEnginePair(t, Config{}, EngineNative, EngineNative)
	const chunk = 4 << 10
	want := make([]byte, 16*chunk)
	newStream(37).rng.Read(want)
	for p := want; len(p) > 0; p = p[chunk:] {
		if _, err := client.Write(p[:chunk]); err != nil {
			t.Fatalf("Write error: %v", err)
		}
	}
	got := make([]byte, len(want))
	if _, err := io.ReadFull(server, got); err != nil {
		t.Fatalf("peer ReadFull error: %v", err)
	}
	// Ample time for the acknowledgements of the peer's device to complete
	// the sends.
	time.Sleep(100 * time.Millisecond)
	if (sendQueueProbe{client}).sendsDone() {
		t.Fatal("the completions of the sends were taken before Close")
	}
	start := time.Now()
	if err := client.Close(); err != nil {
		t.Fatalf("Close error: %v", err)
	}
	elapsed := time.Since(start)
	t.Logf("Close with the completions of %d sends waiting took %v", len(want)/chunk, elapsed)
	if elapsed >= closeTimeout {
		t.Errorf("Close with the completions of its sends waiting took %v, want less than %v", elapsed, closeTimeout)
	}
	server.SetReadDeadline(time.Now().Add(5 * time.Second))
	if rest, err := io.ReadAll(server); len(rest) != 0 || err != nil || !bytes.Equal(got, want) {
		t.Errorf("peer read %d more bytes and %v after Close, what it read before intact: %v; want none, io.EOF and true", len(rest), err, bytes.Equal(got, want))
	}
}

// Connections of the native engine dialed, used both ways and closed again
// and again leave no completion channel open: each destroys the completion
// queues of both directions, without which its channel could not go.
func TestRXESendQueueReleasedWithTheConnection(t *testing.T) {
	rdmatest.Device(t)
	// The descriptors of completion channels, and of the asynchronous events
	// of the devices open, which stay.
	events := func() int {
		fds, err := os.ReadDir("/proc/self/fd")
		if err != nil {
			t.Fatal(err)
		}
		n := 0
		for _, fd := range fds {
			if target, err := os.Readlink("/proc/self/fd/" + fd.Name()); err == nil && strings.Contains(target, "infinibandevent") {
				n++
			}
		}
		return n
	}
	connection := func() {
		client, server := rxeEnginePair(t, Config{}, EngineNative, EngineNative)
		buf := make([]byte, 1)
		for _, ends := range [][2]*Conn{{client, server}, {server, client}} {
			if _, err := ends[0].Write([]byte("x")); err != nil {
				t.Fatalf("Write error: %v", err)
			}
			if _, err := io.ReadFull(ends[1], buf); err != nil {
				t.Fatalf("ReadFull error: %v", err)
			}
		}
		for _, c := range []*Conn{client, server} {
			if err := c.Close(); err != nil {
				t.Fatalf("Close error: %v", err)
			}
		}
	}
	// The first connection may open what the process keeps.
	connection()
	before := events()
	const n = 8
	for range n {
		connection()
	}
	if after := events(); after != before {
		t.Errorf("%d event descriptors of the devices open before %d connections, %d once they were closed", before, n, after)
	}
}
