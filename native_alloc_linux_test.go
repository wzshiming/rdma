//go:build linux && !android && !race && (amd64 || arm64 || loong64 || ppc64le || riscv64 || s390x)

package rdma

import (
	"errors"
	"io"
	"reflect"
	"runtime"
	"testing"
)

// Under the race detector sync.Pool, which dynlib draws the argument block
// of every call from, drops entries at random, so these tests stay out of
// race builds.

// In steady state the Read and Write of a native connection and the rounds
// of its ReadFrom and WriteTo allocate nothing on the goroutine that calls
// them.
func TestRXENativeRoundsDoNotAllocate(t *testing.T) {
	client, server := rxeEnginePair(t, Config{ReceiveBufferSize: 1 << 20}, EngineNative, EngineNative)
	testRoundsDoNotAllocate(t, client, server, registeredMemory)
}

// testRoundsDoNotAllocate checks that in steady state a Write with the Read
// of what it wrote, a round of ReadFrom through a view of client's send
// memory and one of WriteTo through a view of server's receive memory, as
// registered reports them, allocate nothing on the goroutine that calls
// them: twice the rounds cost it as many allocations. The allocations of the
// rest of the process are logged. server's receive buffer must hold 128 KiB.
func testRoundsDoNotAllocate(t *testing.T, client, server *Conn, registered func(*Conn) (send, recv []byte)) {
	if caller, _ := callerAllocs(func() { allocSink = new([64]byte) }); caller != 1 {
		t.Fatalf("the heap profile shows %d allocations under a caller that made one", caller)
	}
	const chunk = 1 << 10
	buf := make([]byte, 128*chunk)
	send, _ := registered(client)
	_, recv := registered(server)
	src := &lentSource{chunk: chunk, mem: send}
	left := 0
	dst := writerFunc(func(p []byte) (int, error) {
		if !within(p, recv) {
			return 0, errForeign
		}
		if left == 0 {
			return len(p), errStop
		}
		left--
		if _, err := client.Write(buf[:chunk]); err != nil {
			return 0, err
		}
		return len(p), nil
	})
	for _, tt := range []struct {
		name   string
		rounds func(n int)
	}{
		{"Write and Read", func(n int) {
			for range n {
				if _, err := client.Write(buf[:chunk]); err != nil {
					t.Fatalf("Write error: %v", err)
				}
				if _, err := io.ReadFull(server, buf[:chunk]); err != nil {
					t.Fatalf("ReadFull error: %v", err)
				}
			}
		}},
		{"ReadFrom", func(n int) {
			src.rounds, src.n = n, 0
			if _, err := client.ReadFrom(src); err != nil {
				t.Fatalf("ReadFrom error: %v", err)
			}
			if _, err := io.ReadFull(server, buf[:src.n]); err != nil {
				t.Fatalf("ReadFull error: %v", err)
			}
		}},
		{"WriteTo", func(n int) {
			left = n
			if _, err := client.Write(buf[:chunk]); err != nil {
				t.Fatalf("Write error: %v", err)
			}
			if _, err := server.WriteTo(dst); !errors.Is(err, errStop) {
				t.Fatalf("WriteTo error = %v, want %v", err, errStop)
			}
		}},
	} {
		few, fewAll := callerAllocs(func() { tt.rounds(64) })
		many, manyAll := callerAllocs(func() { tt.rounds(128) })
		t.Logf("%s: %d and %d allocations on the caller's goroutine for 64 and 128 rounds, %d and %d in the process", tt.name, few, many, fewAll, manyAll)
		if many != few {
			t.Errorf("%s: %d and %d allocations on the caller's goroutine for 64 and 128 rounds; want as many", tt.name, few, many)
		}
	}
}

// lentSource is a source of rounds reads of at most chunk bytes into views
// of mem, which fails on any other buffer; n counts the bytes read.
type lentSource struct {
	rounds, chunk, n int
	mem              []byte
}

func (s *lentSource) Read(p []byte) (int, error) {
	if !within(p, s.mem) {
		return 0, errForeign
	}
	if s.rounds == 0 {
		return 0, io.EOF
	}
	s.rounds--
	n := min(len(p), s.chunk)
	s.n += n
	return n, nil
}

// allocSink keeps what the check of callerAllocs allocates on the heap.
var allocSink *[64]byte

// callerAllocs runs f twice with a single P and every heap allocation
// profiled, the first time to warm up, and returns how many allocations the
// second run made on f's own goroutine, which the profile tells by their
// stacks through allocating, and how many the process made meanwhile.
func callerAllocs(f func()) (caller, all uint64) {
	defer runtime.GOMAXPROCS(runtime.GOMAXPROCS(1))
	defer func(rate int) { runtime.MemProfileRate = rate }(runtime.MemProfileRate)
	runtime.MemProfileRate = 1
	f()
	before := profiledAllocs()
	var m0, m1 runtime.MemStats
	runtime.ReadMemStats(&m0)
	allocating(f)
	runtime.ReadMemStats(&m1)
	return profiledAllocs() - before, m1.Mallocs - m0.Mallocs
}

// allocating runs f, under which the heap profile then shows f's
// allocations.
//
//go:noinline
func allocating(f func()) { f() }

// profiledAllocs returns how many allocations the heap profile shows under
// allocating.
func profiledAllocs() uint64 {
	// The profile takes in an allocation once a collection has followed it.
	runtime.GC()
	runtime.GC()
	n, _ := runtime.MemProfile(nil, true)
	var records []runtime.MemProfileRecord
	for {
		records = make([]runtime.MemProfileRecord, n+50)
		var ok bool
		if n, ok = runtime.MemProfile(records, true); ok {
			records = records[:n]
			break
		}
	}
	marker := runtime.FuncForPC(reflect.ValueOf(allocating).Pointer()).Name()
	var total uint64
	for _, r := range records {
		frames := runtime.CallersFrames(r.Stack())
		for {
			f, more := frames.Next()
			if f.Function == marker {
				total += uint64(r.AllocObjects)
				break
			}
			if !more {
				break
			}
		}
	}
	return total
}
