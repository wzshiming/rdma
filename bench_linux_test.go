//go:build linux && !android && (amd64 || arm64 || loong64 || ppc64le || riscv64 || s390x)

package rdma

import (
	"fmt"
	"io"
	"testing"
	"unsafe"

	"github.com/wzshiming/rdma/internal/rdmatest"
)

// BenchmarkRXERoundTrip measures the round trip of size bytes: a Write, which
// the peer reads in full and writes back, and a ReadFull of the echo.
func BenchmarkRXERoundTrip(b *testing.B) {
	benchOnEachEngine(b, []int{64, 4 << 10}, func(b *testing.B, e Engine, size int) {
		client, server := rxeEnginePair(b, Config{}, e, e)
		benchRoundTrip(b, client, server, size)
	})
}

// BenchmarkRXEWriteRead measures the throughput one way of a loop of Writes
// of size bytes against a loop of Reads into size bytes.
func BenchmarkRXEWriteRead(b *testing.B) {
	benchOnEachEngine(b, throughputSizes, func(b *testing.B, e Engine, size int) {
		client, server := rxeEnginePair(b, Config{}, e, e)
		benchWriteRead(b, client, server, size)
	})
}

// BenchmarkRXEReadFromWriteTo measures the throughput one way of the copy
// paths, ReadFrom on the sending end and WriteTo on the receiving one, and
// fails unless they take the path of the engine: views of the registered
// memory on the native engine, the generic copy on librdmacm's.
func BenchmarkRXEReadFromWriteTo(b *testing.B) {
	benchOnEachEngine(b, throughputSizes, func(b *testing.B, e Engine, size int) {
		client, server := rxeEnginePair(b, Config{}, e, e)
		src, dst := copyPath(e, client, server)
		benchReadFromWriteTo(b, client, server, size, src, dst)
	})
}

var throughputSizes = []int{4 << 10, 64 << 10, 1 << 20}

// benchEngines are the engines the benchmarks compare, librdmacm's first:
// benchstat -col /engine takes the first column it reads as the base.
var benchEngines = []Engine{EngineLibrdmacm, EngineNative}

// benchOnEachEngine runs bench for each size on each engine in turn, as the
// sub-benchmarks size=<bytes>/engine=<engine>. Without a device it skips b,
// as the tests do.
func benchOnEachEngine(b *testing.B, sizes []int, bench func(b *testing.B, e Engine, size int)) {
	rdmatest.Device(b)
	for _, size := range sizes {
		b.Run(fmt.Sprintf("size=%d", size), func(b *testing.B) {
			for _, e := range benchEngines {
				b.Run("engine="+string(e), func(b *testing.B) { bench(b, e, size) })
			}
		})
	}
}

// benchRoundTrip measures round trips of size bytes from client: a Write,
// which server reads in full and writes back, and a ReadFull of the echo.
func benchRoundTrip(b *testing.B, client, server *Conn, size int) {
	echo := async(func() (int64, error) {
		buf := make([]byte, size)
		var n int64
		for {
			if _, err := io.ReadFull(server, buf); err != nil {
				if err == io.EOF {
					err = nil
				}
				return n, err
			}
			if _, err := server.Write(buf); err != nil {
				return n, err
			}
			n += int64(size)
		}
	})
	msg := make([]byte, size)
	b.ResetTimer()
	for range b.N {
		if _, err := client.Write(msg); err != nil {
			b.Fatalf("Write error: %v", err)
		}
		if _, err := io.ReadFull(client, msg); err != nil {
			b.Fatalf("ReadFull error: %v", err)
		}
	}
	b.StopTimer()
	if err := client.CloseWrite(); err != nil {
		b.Fatalf("CloseWrite error: %v", err)
	}
	if r, want := <-echo, int64(b.N*size); r.n != want || r.err != nil {
		b.Fatalf("the echo = %d bytes, %v; want %d and EOF", r.n, r.err, want)
	}
}

// benchWriteRead measures the throughput from client to server of a loop of
// Writes of size bytes against a loop of Reads into size bytes.
func benchWriteRead(b *testing.B, client, server *Conn, size int) {
	read := async(func() (int64, error) {
		buf := make([]byte, size)
		var n int64
		for {
			m, err := server.Read(buf)
			n += int64(m)
			if err == io.EOF {
				return n, nil
			}
			if err != nil {
				return n, err
			}
		}
	})
	msg := make([]byte, size)
	b.SetBytes(int64(size))
	b.ResetTimer()
	for range b.N {
		if _, err := client.Write(msg); err != nil {
			b.Fatalf("Write error: %v", err)
		}
	}
	if err := client.CloseWrite(); err != nil {
		b.Fatalf("CloseWrite error: %v", err)
	}
	r := <-read
	b.StopTimer()
	if want := int64(b.N * size); r.n != want || r.err != nil {
		b.Fatalf("the Read loop = %d bytes, %v; want %d and EOF", r.n, r.err, want)
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
	b.ResetTimer()
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
	b.StopTimer()
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
func copyPath(e Engine, client, server *Conn) (src, dst *pathCheck) {
	if e == EngineNative {
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
