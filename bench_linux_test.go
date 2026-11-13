//go:build linux && !android && (amd64 || arm64 || loong64 || ppc64le || riscv64 || s390x)

package rdma

import (
	"fmt"
	"io"
	"runtime"
	"sync/atomic"
	"testing"
	"time"
	"unsafe"

	"github.com/wzshiming/rdma/internal/rdmatest"
)

// BenchmarkRXERoundTrip measures the round trip of size bytes: a Write, which
// the peer reads in full and writes back, and a ReadFull of the echo; on
// rstream an iteration of a transfer each way.
func BenchmarkRXERoundTrip(b *testing.B) {
	benchOnEachEngine(b, sizeRows(64, 4<<10), benchEngines, rstreamRoundTrip, func(b *testing.B, e engineName, cfg Config, size int) {
		client, server := rxeEnginePair(b, cfg, e, e)
		benchRoundTrip(b, client, server, size)
	})
}

// BenchmarkRXEWriteRead measures the throughput one way of a loop of Writes
// of size bytes against a loop of Reads into size bytes; on rstream one
// iteration of b.N transfers each way in turn. Writes of 64 bytes weigh the
// cost of a message, largeBufferRow the window of bulk transfers.
func BenchmarkRXEWriteRead(b *testing.B) {
	rows := append(sizeRows(64), sizeRows(throughputSizes...)...)
	benchOnEachEngine(b, append(rows, largeBufferRow), benchEngines, rstreamStream, func(b *testing.B, e engineName, cfg Config, size int) {
		client, server := rxeEnginePair(b, cfg, e, e)
		benchWriteRead(b, client, server, size)
	})
}

// BenchmarkRXEReadFromWriteTo measures the throughput one way of the copy
// paths, ReadFrom on the sending end and WriteTo on the receiving one, and
// fails unless they take the path of the engine: views of the registered
// memory on the native engine, the generic copy on librdmacm's.
func BenchmarkRXEReadFromWriteTo(b *testing.B) {
	benchOnEachEngine(b, append(sizeRows(throughputSizes...), largeBufferRow), benchEngines, noRstream, func(b *testing.B, e engineName, cfg Config, size int) {
		client, server := rxeEnginePair(b, cfg, e, e)
		src, dst := copyPath(e, client, server)
		benchReadFromWriteTo(b, client, server, size, src, dst)
	})
}

// BenchmarkRXEDuplex measures the throughput of both directions at once,
// each end a loop of Writes of size bytes against the other's loop of Reads,
// on the native engine alone: the librdmacm engine's at times wedged the
// process on rxe.
func BenchmarkRXEDuplex(b *testing.B) {
	benchOnEachEngine(b, sizeRows(64<<10), []engineName{engineNative}, noRstream, func(b *testing.B, e engineName, cfg Config, size int) {
		client, server := rxeEnginePair(b, cfg, e, e)
		benchDuplex(b, client, server, size)
	})
}

// BenchmarkRXEConcurrentRoundTrip measures round trips of size bytes on more
// connections than CPUs, two pairs per CPU at once. On the librdmacm engine
// it skips before rdma-core 53, as the tests of many connections do.
func BenchmarkRXEConcurrentRoundTrip(b *testing.B) {
	benchOnEachEngine(b, sizeRows(64), benchEngines, noRstream, func(b *testing.B, e engineName, cfg Config, size int) {
		manyConnections(b, e)
		benchConcurrentRoundTrip(b, func() (client, server *Conn) {
			return rxeEnginePair(b, cfg, e, e)
		}, size)
	})
}

// BenchmarkRXEConnect measures a connection from its dial to its close, one
// at a time: the dial, the accept of it on a listener of the same engine,
// and the close of both ends.
func BenchmarkRXEConnect(b *testing.B) {
	benchOnEachEngine(b, []benchRow{{name: "conns=1"}}, benchEngines, noRstream, func(b *testing.B, e engineName, cfg Config, _ int) {
		benchConnect(b, rxeListenOn(b, cfg, e.engine()), Dialer{Config: cfg, Timeout: rxeTimeout, eng: e.engine()})
	})
}

// BenchmarkRXEPolledRoundTrip measures the round trips of 64 B and 4 KiB of
// BenchmarkRXERoundTrip on the native engine alone, between connections of
// polledRows. The polled benchmarks come last, leaving the others to run as
// they did before them.
func BenchmarkRXEPolledRoundTrip(b *testing.B) {
	benchOnEachEngine(b, polledRows(64, 4<<10), []engineName{engineNative}, noRstream, func(b *testing.B, e engineName, cfg Config, size int) {
		client, server := rxeEnginePair(b, cfg, e, e)
		benchRoundTrip(b, client, server, size)
	})
}

// BenchmarkRXEPolledWriteRead measures the stream of 64 B Writes of
// BenchmarkRXEWriteRead on the native engine alone, between connections of
// polledRows.
func BenchmarkRXEPolledWriteRead(b *testing.B) {
	benchOnEachEngine(b, polledRows(64), []engineName{engineNative}, noRstream, func(b *testing.B, e engineName, cfg Config, size int) {
		client, server := rxeEnginePair(b, cfg, e, e)
		benchWriteRead(b, client, server, size)
	})
}

var throughputSizes = []int{4 << 10, 64 << 10, 1 << 20}

// largeBufferRow moves 1 MiB an op between connections whose send and
// receive buffers are 1 MiB, which rsockets double, where those of the zero
// Config register 128 KiB.
var largeBufferRow = benchRow{"buffer=1048576", 1 << 20, Config{SendBufferSize: 1 << 20, ReceiveBufferSize: 1 << 20}}

// benchEngines are the engines the benchmarks compare, librdmacm's first:
// benchstat -col /engine takes the first column it reads as the base.
var benchEngines = []engineName{engineLibrdmacm, engineNative}

// benchRow is a row of a benchmark, the first level of its sub-benchmarks:
// ops of size bytes between connections of cfg.
type benchRow struct {
	name string
	size int
	cfg  Config
}

// sizeRows are the rows size=<bytes> of each size with the zero Config.
func sizeRows(sizes ...int) []benchRow {
	rows := make([]benchRow, len(sizes))
	for i, size := range sizes {
		rows[i] = benchRow{fmt.Sprintf("size=%d", size), size, Config{}}
	}
	return rows
}

// polledRows are the rows size=<bytes> of each size between connections
// whose waits poll for 10 µs before they sleep, as rsockets do by default.
func polledRows(sizes ...int) []benchRow {
	rows := sizeRows(sizes...)
	for i := range rows {
		rows[i].cfg.PollTime = 10 * time.Microsecond
	}
	return rows
}

// benchOnEachEngine runs bench for each row on each of engines in turn, as
// the sub-benchmarks <row>/engine=<engine>, with the Config of the row, and
// then the row's ops of op between two rstreams, as <row>/engine=rstream,
// unless op is noRstream. Without a device it skips b, as the tests do.
func benchOnEachEngine(b *testing.B, rows []benchRow, engines []engineName, op rstreamOp, bench func(b *testing.B, e engineName, cfg Config, size int)) {
	rdmatest.Device(b)
	for _, row := range rows {
		b.Run(row.name, func(b *testing.B) {
			for _, e := range engines {
				b.Run("engine="+string(e), func(b *testing.B) { bench(b, e, row.cfg, row.size) })
			}
			if op != noRstream {
				b.Run("engine=rstream", func(b *testing.B) { benchRstream(b, op, row.cfg, row.size) })
			}
		})
	}
}

// Without a device every benchmark skips, and the rstream column on its own,
// or fails under RDMA_TEST_REQUIRE=1.
func TestBenchmarksWithoutDevice(t *testing.T) {
	for _, bench := range []struct {
		name string
		f    func(*testing.B)
	}{
		{"BenchmarkRXERoundTrip", BenchmarkRXERoundTrip},
		{"BenchmarkRXEWriteRead", BenchmarkRXEWriteRead},
		{"BenchmarkRXEReadFromWriteTo", BenchmarkRXEReadFromWriteTo},
		{"BenchmarkRXEDuplex", BenchmarkRXEDuplex},
		{"BenchmarkRXEConcurrentRoundTrip", BenchmarkRXEConcurrentRoundTrip},
		{"BenchmarkRXEConnect", BenchmarkRXEConnect},
		{"BenchmarkRXEPolledRoundTrip", BenchmarkRXEPolledRoundTrip},
		{"BenchmarkRXEPolledWriteRead", BenchmarkRXEPolledWriteRead},
		{"rstream column", func(b *testing.B) { benchRstream(b, rstreamRoundTrip, Config{}, 64) }},
	} {
		for _, tt := range []struct {
			require            string
			wantSkip, wantFail bool
		}{
			{"", true, false},
			{"1", false, true},
		} {
			t.Run(bench.name+"/RDMA_TEST_REQUIRE="+tt.require, func(t *testing.T) {
				t.Setenv("RDMA_TEST_DEVICE", "absent0")
				t.Setenv("RDMA_TEST_REQUIRE", tt.require)
				if skipped, failed := benchOutcome(bench.f); skipped != tt.wantSkip || failed != tt.wantFail {
					t.Errorf("skipped=%v failed=%v, want skipped=%v failed=%v", skipped, failed, tt.wantSkip, tt.wantFail)
				}
			})
		}
	}
}

// benchResult runs bench as a benchmark of its own and returns its result,
// failing t unless it passed and reported the counters of startMeter.
func benchResult(t *testing.T, bench func(b *testing.B)) testing.BenchmarkResult {
	t.Helper()
	var failed bool
	res := testing.Benchmark(func(b *testing.B) {
		defer func() { failed = b.Failed() }()
		bench(b)
	})
	if failed {
		t.Fatal("the benchmark failed")
	}
	for _, unit := range []string{"syscr/op", "syscw/op", "cpu-ns/op", "nvcsw/op"} {
		if _, ok := res.Extra[unit]; !ok {
			t.Errorf("the benchmark reported no %s", unit)
		}
	}
	return res
}

// A round trip on a pair of the fake engine counts the bytes of both ways.
func TestBenchRoundTripOnFakeEngine(t *testing.T) {
	res := benchResult(t, func(b *testing.B) {
		client, server := fakeEngines[0].pair(b, Config{})
		benchRoundTrip(b, client, server, 64)
	})
	if res.Bytes != 2*64 {
		t.Errorf("an op counts %d bytes, want 128, 64 each way", res.Bytes)
	}
}

// A write-read stream on a pair of the fake engine counts the bytes of a
// Write.
func TestBenchWriteReadOnFakeEngine(t *testing.T) {
	res := benchResult(t, func(b *testing.B) {
		client, server := fakeEngines[0].pair(b, Config{})
		benchWriteRead(b, client, server, 4<<10)
	})
	if res.Bytes != 4<<10 {
		t.Errorf("an op counts %d bytes, want 4096, a Write's", res.Bytes)
	}
}

// The duplex on a pair of the fake engine counts the bytes of both ways.
func TestBenchDuplexOnFakeEngine(t *testing.T) {
	res := benchResult(t, func(b *testing.B) {
		client, server := fakeEngines[0].pair(b, Config{})
		benchDuplex(b, client, server, 4<<10)
	})
	if res.Bytes != 2*4<<10 {
		t.Errorf("an op counts %d bytes, want 8192, 4096 each way", res.Bytes)
	}
}

// Concurrent round trips on the fake engine run on two pairs per CPU and
// count the bytes of both ways.
func TestBenchConcurrentRoundTripOnFakeEngine(t *testing.T) {
	var pairs int
	res := benchResult(t, func(b *testing.B) {
		pairs = 0
		benchConcurrentRoundTrip(b, func() (client, server *Conn) {
			pairs++
			return fakeEngines[0].pair(b, Config{})
		}, 64)
	})
	if want := 2 * runtime.GOMAXPROCS(0); pairs != want {
		t.Errorf("the round trips ran on %d pairs, want two per CPU, %d", pairs, want)
	}
	if res.Bytes != 2*64 {
		t.Errorf("an op counts %d bytes, want 128, 64 each way", res.Bytes)
	}
}

// Each connection of the connect benchmark on the fake network is accepted
// and closed at both ends within its op.
func TestBenchConnectOnFakeNetwork(t *testing.T) {
	var open int
	benchResult(t, func(b *testing.B) {
		nw := newFakeNet(b)
		benchConnect(b, listenFake(b, nw, Config{}), Dialer{eng: librdmacmEngine{nw}})
		open = nw.openSockets()
	})
	if open != 1 {
		t.Errorf("%d sockets open after the connections, want the listener's alone", open)
	}
}

// benchRoundTrip measures round trips of size bytes from client: a Write,
// which server reads in full and writes back, and a ReadFull of the echo,
// size bytes each way.
func benchRoundTrip(b *testing.B, client, server *Conn, size int) {
	echoed := async(func() (int64, error) { return echo(server, size) })
	msg := make([]byte, size)
	b.SetBytes(int64(2 * size))
	stop := startMeter(b)
	for range b.N {
		if _, err := client.Write(msg); err != nil {
			b.Fatalf("Write error: %v", err)
		}
		if _, err := io.ReadFull(client, msg); err != nil {
			b.Fatalf("ReadFull error: %v", err)
		}
	}
	stop()
	if err := client.CloseWrite(); err != nil {
		b.Fatalf("CloseWrite error: %v", err)
	}
	if r, want := <-echoed, int64(b.N*size); r.n != want || r.err != nil {
		b.Fatalf("the echo = %d bytes, %v; want %d and EOF", r.n, r.err, want)
	}
}

// echo reads size bytes at a time from c in full and writes them back until
// io.EOF, and returns the bytes it echoed.
func echo(c *Conn, size int) (int64, error) {
	buf := make([]byte, size)
	var n int64
	for {
		if _, err := io.ReadFull(c, buf); err != nil {
			if err == io.EOF {
				err = nil
			}
			return n, err
		}
		if _, err := c.Write(buf); err != nil {
			return n, err
		}
		n += int64(size)
	}
}

// benchConcurrentRoundTrip measures round trips of size bytes, as
// benchRoundTrip does, on two pairs per CPU at once: each client makes its
// round trips on a goroutine of RunParallel, each server echoes on one of
// its own.
func benchConcurrentRoundTrip(b *testing.B, pair func() (client, server *Conn), size int) {
	const perCPU = 2
	clients := make([]*Conn, perCPU*runtime.GOMAXPROCS(0))
	echoed := make([]<-chan copied, len(clients))
	for i := range clients {
		var server *Conn
		clients[i], server = pair()
		echoed[i] = async(func() (int64, error) { return echo(server, size) })
	}
	var next atomic.Int64
	b.SetParallelism(perCPU)
	b.SetBytes(int64(2 * size))
	stop := startMeter(b)
	b.RunParallel(func(pb *testing.PB) {
		client := clients[next.Add(1)-1]
		msg := make([]byte, size)
		for pb.Next() {
			if _, err := client.Write(msg); err != nil {
				b.Errorf("Write error: %v", err)
				return
			}
			if _, err := io.ReadFull(client, msg); err != nil {
				b.Errorf("ReadFull error: %v", err)
				return
			}
		}
	})
	stop()
	if b.Failed() {
		return
	}
	var n int64
	for i, client := range clients {
		if err := client.CloseWrite(); err != nil {
			b.Fatalf("CloseWrite error: %v", err)
		}
		r := <-echoed[i]
		if r.err != nil {
			b.Fatalf("echo error: %v", r.err)
		}
		n += r.n
	}
	if want := int64(b.N * size); n != want {
		b.Fatalf("the echoes = %d bytes, want %d", n, want)
	}
}

// benchConnect measures the connections d dials to ln, one at a time: the
// dial, the accept of it, and the close of both ends.
func benchConnect(b *testing.B, ln *Listener, d Dialer) {
	addr := ln.Addr().String()
	stop := startMeter(b)
	for range b.N {
		acc := acceptAsync(ln)
		client, err := d.Dial("rdma", addr)
		if err != nil {
			b.Fatalf("Dial(%v) error: %v", addr, err)
		}
		r := <-acc
		if r.err != nil {
			b.Fatalf("AcceptRDMA() error: %v", r.err)
		}
		if err := client.Close(); err != nil {
			b.Fatalf("Close of the dialed end error: %v", err)
		}
		if err := r.c.Close(); err != nil {
			b.Fatalf("Close of the accepted end error: %v", err)
		}
	}
	stop()
}

// benchWriteRead measures the throughput from client to server of a loop of
// Writes of size bytes against a loop of Reads into size bytes.
func benchWriteRead(b *testing.B, client, server *Conn, size int) {
	read := async(func() (int64, error) { return readToEOF(server, size) })
	msg := make([]byte, size)
	b.SetBytes(int64(size))
	stop := startMeter(b)
	if err := writeAndClose(client, msg, b.N); err != nil {
		b.Fatal(err)
	}
	r := <-read
	stop()
	if want := int64(b.N * size); r.n != want || r.err != nil {
		b.Fatalf("the Read loop = %d bytes, %v; want %d and EOF", r.n, r.err, want)
	}
}

// writeAndClose writes msg n times to c and then closes its writing side.
func writeAndClose(c *Conn, msg []byte, n int) error {
	for range n {
		if _, err := c.Write(msg); err != nil {
			return fmt.Errorf("Write error: %w", err)
		}
	}
	if err := c.CloseWrite(); err != nil {
		return fmt.Errorf("CloseWrite error: %w", err)
	}
	return nil
}

// readToEOF reads c into a buffer of size bytes until io.EOF and returns the
// bytes it read.
func readToEOF(c *Conn, size int) (int64, error) {
	buf := make([]byte, size)
	var n int64
	for {
		m, err := c.Read(buf)
		n += int64(m)
		if err == io.EOF {
			return n, nil
		}
		if err != nil {
			return n, err
		}
	}
}

// benchDuplex measures the throughput of both directions at once: client and
// server each write size bytes an op, closing their writing side after the
// last, while each reads what the other writes.
func benchDuplex(b *testing.B, client, server *Conn, size int) {
	toServer := async(func() (int64, error) { return readToEOF(server, size) })
	toClient := async(func() (int64, error) { return readToEOF(client, size) })
	up, down := make([]byte, size), make([]byte, size)
	wrote := make(chan error, 1)
	b.SetBytes(int64(2 * size))
	stop := startMeter(b)
	go func() { wrote <- writeAndClose(server, down, b.N) }()
	if err := writeAndClose(client, up, b.N); err != nil {
		b.Fatalf("client: %v", err)
	}
	if err := <-wrote; err != nil {
		b.Fatalf("server: %v", err)
	}
	read := [...]copied{<-toServer, <-toClient}
	stop()
	for i, end := range [...]string{"server", "client"} {
		if r, want := read[i], int64(b.N*size); r.n != want || r.err != nil {
			b.Fatalf("the Read loop of the %s = %d bytes, %v; want %d and EOF", end, r.n, r.err, want)
		}
	}
}

// benchReadFromWriteTo measures the throughput from client to server of
// ReadFrom, whose source hands over at most size bytes a read, against
// WriteTo, whose destination takes all it is handed. Neither touches the
// bytes, so that the figures are those of the path between them, which src
// and dst check on every buffer the source and the destination are handed.
func benchReadFromWriteTo(b *testing.B, client, server *Conn, size int, src, dst *pathCheck) {
	wrote := async(func() (int64, error) {
		return server.WriteTo(writerFunc(func(p []byte) (int, error) {
			dst.check(p)
			return len(p), nil
		}))
	})
	total := b.N * size
	left := total
	b.SetBytes(int64(size))
	stop := startMeter(b)
	n, err := client.ReadFrom(readerFunc(func(p []byte) (int, error) {
		src.check(p)
		if left == 0 {
			return 0, io.EOF
		}
		k := min(len(p), size, left)
		left -= k
		return k, nil
	}))
	if n != int64(total) || err != nil {
		b.Fatalf("ReadFrom = %d, %v; want %d and nil", n, err, total)
	}
	if err := client.CloseWrite(); err != nil {
		b.Fatalf("CloseWrite error: %v", err)
	}
	w := <-wrote
	stop()
	if w.n != int64(total) || w.err != nil {
		b.Fatalf("WriteTo = %d, %v; want %d and nil", w.n, w.err, total)
	}
	src.verify(b, "ReadFrom handed the source")
	dst.verify(b, "WriteTo handed the destination")
}

// pathCheck checks the buffers a copy hands its source or its destination
// against the path the copy must take.
type pathCheck struct {
	path   string              // the path, as a failure names it
	on     func(p []byte) bool // whether p lies on the path
	handed int                 // the buffers handed
	off    int                 // those of them off the path
}

func (c *pathCheck) check(p []byte) {
	c.handed++
	if !c.on(p) {
		c.off++
	}
}

// verify fails b unless the copy handed out buffers, all of them on the path.
func (c *pathCheck) verify(b *testing.B, what string) {
	b.Helper()
	switch {
	case c.handed == 0:
		b.Errorf("%s no buffer", what)
	case c.off > 0:
		b.Errorf("%s %d of %d buffers off the path of %s", what, c.off, c.handed, c.path)
	}
}

// copyPath returns the checks of the buffers ReadFrom hands its source on
// client and WriteTo its destination on server against the path of the
// engine e: the native engine lends the memory the connections registered,
// librdmacm's takes the generic copy.
func copyPath(e engineName, client, server *Conn) (src, dst *pathCheck) {
	if e == engineNative {
		send, _ := registeredMemory(client)
		_, recv := registeredMemory(server)
		return lentFrom("the registered send memory", send), lentFrom("the registered receive memory", recv)
	}
	return genericCopy(), genericCopy()
}

// lentFrom is the path of a copy that lends views of mem.
func lentFrom(what string, mem []byte) *pathCheck {
	return &pathCheck{path: "views of " + what, on: func(p []byte) bool { return within(p, mem) }}
}

// genericCopy is the path of the generic copy, which hands out the same
// buffer of copyBufferSize bytes of its own every time.
func genericCopy() *pathCheck {
	var buf *byte
	return &pathCheck{
		path: fmt.Sprintf("one buffer of %d bytes of the copy's own", copyBufferSize),
		on: func(p []byte) bool {
			if buf == nil {
				buf = unsafe.SliceData(p)
			}
			return unsafe.SliceData(p) == buf && len(p) <= copyBufferSize
		},
	}
}
