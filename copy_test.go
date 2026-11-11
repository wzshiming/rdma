package rdma

import (
	"bytes"
	"errors"
	"io"
	"math/rand/v2"
	"net"
	"os"
	"strings"
	"sync"
	"testing"
	"time"

	"golang.org/x/net/nettest"
)

// engineUnderTest is an engine the copy-path suites run on.
type engineUnderTest struct {
	name string
	// new returns a fresh engine for t.
	new func(t testing.TB) engine
	// addr is the address to listen on, host:port.
	addr string
	// owns reports whether p lies in the buffers of s; nil for an engine
	// whose streams do not lend.
	owns func(s stream, p []byte) bool
}

// fakeEngines are the two forms of the fake engine.
var fakeEngines = []engineUnderTest{
	{name: "lending", new: newLendingFake, addr: "10.99.0.1:0", owns: fakeOwns},
	{name: "copying", new: newCopyingFake, addr: "10.99.0.1:0"},
}

// pair connects a dialer to a listener on a fresh engine of e, both with cfg,
// and returns the two ends.
func (e engineUnderTest) pair(t testing.TB, cfg Config) (client, server *Conn) {
	t.Helper()
	eng := e.new(t)
	lc := ListenConfig{Config: cfg, eng: eng}
	ln, err := lc.Listen(t.Context(), "rdma", e.addr)
	if err != nil {
		t.Fatalf("Listen(%s) error: %v", e.addr, err)
	}
	defer ln.Close()
	acc := acceptAsync(ln)
	d := Dialer{Config: cfg, eng: eng}
	client, err = d.Dial("rdma", ln.Addr().String())
	if err != nil {
		t.Fatalf("Dial(%v) error: %v", ln.Addr(), err)
	}
	t.Cleanup(func() { client.Close() })
	r := <-acc
	if r.err != nil {
		t.Fatalf("AcceptRDMA() error: %v", r.err)
	}
	t.Cleanup(func() { r.c.Close() })
	return client, r.c
}

// testConn runs nettest.TestConn on pairs of e.
func testConn(t *testing.T, e engineUnderTest) {
	nettest.TestConn(t, func() (c1, c2 net.Conn, stop func(), err error) {
		client, server := e.pair(t, Config{})
		return client, server, func() {
			client.Close()
			server.Close()
		}, nil
	})
}

func TestConnOnFakeEngines(t *testing.T) {
	for _, e := range fakeEngines {
		t.Run(e.name, func(t *testing.T) { testConn(t, e) })
	}
}

func TestCopyPathsOnFakeEngines(t *testing.T) {
	for _, e := range fakeEngines {
		t.Run(e.name, func(t *testing.T) { testCopyPaths(t, e) })
	}
}

// testCopyPaths runs the tests of ReadFrom and WriteTo on pairs of e.
func testCopyPaths(t *testing.T, e engineUnderTest) {
	for _, tt := range []struct {
		name string
		test func(*testing.T, engineUnderTest)
	}{
		{"CopyEntersReadFrom", testCopyEntersReadFrom},
		{"CopyEntersWriteTo", testCopyEntersWriteTo},
		{"ReadFromReturnsNilAtEOF", testReadFromReturnsNilAtEOF},
		{"WriteToReturnsNilAtPeerCloseWrite", testWriteToReturnsNilAtPeerCloseWrite},
		{"WriteToReportsShortWrite", testWriteToReportsShortWrite},
		{"CopyBetweenConnsDoesNotRecurse", testCopyBetweenConnsDoesNotRecurse},
		{"CopyPathsRunWithReadAndWrite", testCopyPathsRunWithReadAndWrite},
		{"CopyEcho", testCopyEcho},
		{"CopyIsIntactUnderBackPressure", testCopyIsIntactUnderBackPressure},
		{"CopyKeepsOrderWithReadAndWrite", testCopyKeepsOrderWithReadAndWrite},
		{"CopyLendsEngineBuffers", testCopyLendsEngineBuffers},
		{"WriteToLeavesWhatTheDestinationRefused", testWriteToLeavesWhatTheDestinationRefused},
		{"WriteToFreesRoomOnlyOnceTaken", testWriteToFreesRoomOnlyOnceTaken},
		{"LendingRoundsDoNotAllocate", testLendingRoundsDoNotAllocate},
	} {
		t.Run(tt.name, func(t *testing.T) { tt.test(t, e) })
	}
}

var (
	errSource = errors.New("source failed")
	errDest   = errors.New("destination failed")
	errStop   = errors.New("destination stops")
)

// readerFunc adapts a function to io.Reader, with no WriteTo for io.Copy to
// prefer.
type readerFunc func(p []byte) (int, error)

func (f readerFunc) Read(p []byte) (int, error) { return f(p) }

// writerFunc adapts a function to io.Writer, with no ReadFrom for io.Copy to
// prefer.
type writerFunc func(p []byte) (int, error)

func (f writerFunc) Write(p []byte) (int, error) { return f(p) }

// readsThen returns a reader of data that then reports err.
func readsThen(data string, err error) io.Reader {
	r := strings.NewReader(data)
	return readerFunc(func(p []byte) (int, error) {
		if r.Len() == 0 {
			return 0, err
		}
		return r.Read(p)
	})
}

// isOpError reports whether err is a *net.OpError of op that matches target.
func isOpError(err error, op string, target error) bool {
	var opErr *net.OpError
	return errors.As(err, &opErr) && opErr.Op == op && opErr.Net == "rdma" && errors.Is(err, target)
}

// testCopyEntersReadFrom checks that io.Copy to a connection goes through
// ReadFrom, which sends what the source read before it failed and reports
// the failure in a readfrom *net.OpError.
func testCopyEntersReadFrom(t *testing.T, e engineUnderTest) {
	client, server := e.pair(t, Config{})
	n, err := io.Copy(client, readsThen("hello", errSource))
	if n != 5 || !isOpError(err, "readfrom", errSource) {
		t.Fatalf("io.Copy(conn, src) = %d, %v; want 5 and a readfrom *net.OpError with %v", n, err, errSource)
	}
	got := make([]byte, 5)
	if _, err := io.ReadFull(server, got); err != nil || string(got) != "hello" {
		t.Fatalf("peer read %q, %v; want %q", got, err, "hello")
	}
}

// testCopyEntersWriteTo checks that io.Copy from a connection goes through
// WriteTo, which counts what the destination took before it failed and
// reports the failure in a writeto *net.OpError.
func testCopyEntersWriteTo(t *testing.T, e engineUnderTest) {
	client, server := e.pair(t, Config{})
	if _, err := server.Write([]byte("hello")); err != nil {
		t.Fatalf("Write error: %v", err)
	}
	var got []byte
	dst := writerFunc(func(p []byte) (int, error) {
		got = append(got, p...)
		return len(p), errDest
	})
	n, err := io.Copy(dst, client)
	if n != 5 || string(got) != "hello" || !isOpError(err, "writeto", errDest) {
		t.Fatalf("io.Copy(dst, conn) = %d, %v after handing over %q; want 5 and a writeto *net.OpError with %v after %q", n, err, got, errDest, "hello")
	}
}

func testReadFromReturnsNilAtEOF(t *testing.T, e engineUnderTest) {
	client, server := e.pair(t, Config{})
	n, err := client.ReadFrom(readsThen("hello", io.EOF))
	if n != 5 || err != nil {
		t.Fatalf("ReadFrom() = %d, %v; want 5, nil", n, err)
	}
	if err := client.CloseWrite(); err != nil {
		t.Fatalf("CloseWrite error: %v", err)
	}
	if got, err := io.ReadAll(server); err != nil || string(got) != "hello" {
		t.Fatalf("peer ReadAll = %q, %v; want %q, nil", got, err, "hello")
	}
}

// testWriteToReturnsNilAtPeerCloseWrite checks that WriteTo returns nil once
// the peer has shut down writing and its data is handed over, and that the
// connection still writes back.
func testWriteToReturnsNilAtPeerCloseWrite(t *testing.T, e engineUnderTest) {
	client, server := e.pair(t, Config{})
	if _, err := server.Write([]byte("request")); err != nil {
		t.Fatalf("Write error: %v", err)
	}
	if err := server.CloseWrite(); err != nil {
		t.Fatalf("CloseWrite error: %v", err)
	}
	var got bytes.Buffer
	n, err := client.WriteTo(writerFunc(got.Write))
	if n != 7 || err != nil || got.String() != "request" {
		t.Fatalf("WriteTo() = %d, %v with %q; want 7, nil with %q", n, err, got.String(), "request")
	}
	if _, err := client.Write([]byte("response")); err != nil {
		t.Fatalf("Write after WriteTo error: %v", err)
	}
	if err := client.CloseWrite(); err != nil {
		t.Fatalf("CloseWrite error: %v", err)
	}
	if reply, err := io.ReadAll(server); err != nil || string(reply) != "response" {
		t.Fatalf("peer ReadAll = %q, %v; want %q, nil", reply, err, "response")
	}
}

func testWriteToReportsShortWrite(t *testing.T, e engineUnderTest) {
	client, server := e.pair(t, Config{})
	if _, err := server.Write([]byte("hello")); err != nil {
		t.Fatalf("Write error: %v", err)
	}
	n, err := client.WriteTo(writerFunc(func(p []byte) (int, error) { return min(len(p), 2), nil }))
	if n != 2 || !isOpError(err, "writeto", io.ErrShortWrite) {
		t.Fatalf("WriteTo(a writer taking 2 bytes) = %d, %v; want 2 and a writeto *net.OpError with io.ErrShortWrite", n, err)
	}
}

// within5s runs f and fails t unless it returns within 5 seconds.
func within5s(t *testing.T, what string, f func()) {
	t.Helper()
	done := make(chan struct{})
	go func() {
		defer close(done)
		f()
	}()
	select {
	case <-done:
	case <-time.After(5 * time.Second):
		t.Fatalf("%s did not return", what)
	}
}

// testCopyBetweenConnsDoesNotRecurse copies a connection into another both
// ways round: WriteTo writes to the other connection and ReadFrom reads from
// it, neither coming back to itself.
func testCopyBetweenConnsDoesNotRecurse(t *testing.T, e engineUnderTest) {
	a1, a2 := e.pair(t, Config{})
	b1, b2 := e.pair(t, Config{})
	for _, cp := range []struct {
		name string
		copy func() (int64, error)
	}{
		{"io.Copy(conn, conn)", func() (int64, error) { return io.Copy(b1, a2) }},
		{"ReadFrom(a reader of a conn)", func() (int64, error) { return b1.ReadFrom(readerFunc(a2.Read)) }},
	} {
		if _, err := a1.Write([]byte("ping")); err != nil {
			t.Fatalf("Write error: %v", err)
		}
		a2.SetReadDeadline(time.Now().Add(100 * time.Millisecond))
		var n int64
		var err error
		within5s(t, cp.name, func() { n, err = cp.copy() })
		if n != 4 || !errors.Is(err, os.ErrDeadlineExceeded) {
			t.Errorf("%s = %d, %v; want 4 and the source's deadline error", cp.name, n, err)
		}
		got := make([]byte, 4)
		if _, err := io.ReadFull(b2, got); err != nil || string(got) != "ping" {
			t.Fatalf("after %s the peer read %q, %v; want %q", cp.name, got, err, "ping")
		}
	}
}

// testCopyPathsRunWithReadAndWrite runs ReadFrom alongside Read on one end
// and WriteTo alongside Write on the other.
func testCopyPathsRunWithReadAndWrite(t *testing.T, e engineUnderTest) {
	client, server := e.pair(t, Config{})
	const total = 4 << 20
	inA, outA := newStream(1), newStream(1)
	inB, outB := newStream(2), newStream(2)
	var wg sync.WaitGroup
	wg.Add(4)
	go func() {
		defer wg.Done()
		if _, err := client.ReadFrom(&chunkySource{s: inA, rng: rand.New(rand.NewPCG(1, 0)), left: total}); err != nil {
			t.Errorf("ReadFrom error: %v", err)
		}
		if err := client.CloseWrite(); err != nil {
			t.Errorf("CloseWrite error: %v", err)
		}
	}()
	go func() {
		defer wg.Done()
		if err := outB.readFrom(client, 3000); err != nil {
			t.Errorf("Read error: %v", err)
		}
	}()
	go func() {
		defer wg.Done()
		if _, err := server.WriteTo(&sink{s: outA}); err != nil {
			t.Errorf("WriteTo error: %v", err)
		}
	}()
	go func() {
		defer wg.Done()
		if err := inB.writeTo(server, total, 5000); err != nil {
			t.Errorf("Write error: %v", err)
		}
		if err := server.CloseWrite(); err != nil {
			t.Errorf("CloseWrite error: %v", err)
		}
	}()
	wg.Wait()
	if inA.sum() != outA.sum() {
		t.Errorf("ReadFrom sent %s, WriteTo received %s", inA.sum(), outA.sum())
	}
	if inB.sum() != outB.sum() {
		t.Errorf("Write sent %s, Read received %s", inB.sum(), outB.sum())
	}
}

// testCopyEcho echoes with io.Copy(conn, conn), which runs WriteTo into the
// connection's own Write.
func testCopyEcho(t *testing.T, e engineUnderTest) {
	client, server := e.pair(t, Config{ReceiveBufferSize: 4096})
	go io.Copy(server, server)
	const n = 1 << 20
	want := make([]byte, n)
	newStream(9).rng.Read(want)
	werr := make(chan error, 1)
	go func() {
		_, err := client.Write(want)
		werr <- err
	}()
	got := make([]byte, n)
	if _, err := io.ReadFull(client, got); err != nil {
		t.Fatalf("ReadFull error: %v", err)
	}
	if err := <-werr; err != nil {
		t.Fatalf("Write error: %v", err)
	}
	if !bytes.Equal(got, want) {
		t.Error("echo differs")
	}
}

// chunkySource reads left bytes of the stream s in reads of random sizes:
// none at all, a few bytes, part of the buffer or all of it.
type chunkySource struct {
	s    *testStream
	rng  *rand.Rand
	left int
}

func (c *chunkySource) Read(p []byte) (int, error) {
	if c.left == 0 {
		return 0, io.EOF
	}
	var n int
	switch c.rng.IntN(4) {
	case 0:
	case 1:
		n = 1 + c.rng.IntN(16)
	case 2:
		n = 1 + c.rng.IntN(len(p))
	default:
		n = len(p)
	}
	n = min(n, len(p), c.left)
	c.s.rng.Read(p[:n])
	c.s.hash.Write(p[:n])
	c.s.n += int64(n)
	c.left -= n
	return n, nil
}

// sink hashes what it is given into s, taking all of it; a slow sink sleeps
// now and then.
type sink struct {
	s    *testStream
	slow *rand.Rand
}

func (w *sink) Write(p []byte) (int, error) {
	w.s.hash.Write(p)
	w.s.n += int64(len(p))
	if w.slow != nil && w.slow.IntN(32) == 0 {
		time.Sleep(time.Millisecond)
	}
	return len(p), nil
}

// testCopyIsIntactUnderBackPressure sends 64 MiB from a source of random
// read sizes through ReadFrom to a slow destination behind WriteTo.
func testCopyIsIntactUnderBackPressure(t *testing.T, e engineUnderTest) {
	client, server := e.pair(t, Config{})
	const total = 64 << 20
	in, out := newStream(3), newStream(3)
	errc := make(chan error, 1)
	go func() {
		_, err := client.ReadFrom(&chunkySource{s: in, rng: rand.New(rand.NewPCG(3, 0)), left: total})
		if err == nil {
			err = client.CloseWrite()
		}
		errc <- err
	}()
	if _, err := server.WriteTo(&sink{s: out, slow: rand.New(rand.NewPCG(4, 0))}); err != nil {
		t.Errorf("WriteTo error: %v", err)
	}
	if err := <-errc; err != nil {
		t.Errorf("ReadFrom error: %v", err)
	}
	if in.sum() != out.sum() {
		t.Errorf("sent %s, received %s", in.sum(), out.sum())
	}
}

// testCopyKeepsOrderWithReadAndWrite alternates Write with ReadFrom on one
// end and Read with WriteTo on the other, and checks that the bytes arrive
// once each and in order.
func testCopyKeepsOrderWithReadAndWrite(t *testing.T, e engineUnderTest) {
	client, server := e.pair(t, Config{ReceiveBufferSize: 16 << 10})
	const total = 4 << 20
	in, out := newStream(5), newStream(5)
	errc := make(chan error, 1)
	go func() {
		rng := rand.New(rand.NewPCG(5, 0))
		for left := total; left > 0; {
			k := min(left, 1+rng.IntN(32<<10))
			var err error
			if rng.IntN(2) == 0 {
				err = in.writeTo(client, k, k)
			} else {
				_, err = client.ReadFrom(&chunkySource{s: in, rng: rng, left: k})
			}
			if err != nil {
				errc <- err
				return
			}
			left -= k
		}
		errc <- client.CloseWrite()
	}()
	rng := rand.New(rand.NewPCG(6, 0))
	buf := make([]byte, 32<<10)
	for done := false; !done; {
		if rng.IntN(2) == 0 {
			n, err := server.Read(buf[:1+rng.IntN(len(buf))])
			out.hash.Write(buf[:n])
			out.n += int64(n)
			if err == io.EOF {
				done = true
			} else if err != nil {
				t.Fatalf("Read error: %v", err)
			}
			continue
		}
		limit, taken := 1+rng.IntN(64<<10), 0
		_, err := server.WriteTo(writerFunc(func(p []byte) (int, error) {
			out.hash.Write(p)
			out.n += int64(len(p))
			if taken += len(p); taken >= limit {
				return len(p), errStop
			}
			return len(p), nil
		}))
		if err == nil {
			done = true
		} else if !errors.Is(err, errStop) {
			t.Fatalf("WriteTo error: %v", err)
		}
	}
	if err := <-errc; err != nil {
		t.Errorf("sending side error: %v", err)
	}
	if in.sum() != out.sum() {
		t.Errorf("sent %s, received %s", in.sum(), out.sum())
	}
}

// lending skips t unless e's streams lend their buffers.
func lending(t *testing.T, e engineUnderTest) {
	t.Helper()
	if e.owns == nil {
		t.Skip("the engine does not lend its buffers")
	}
}

// testCopyLendsEngineBuffers checks that every slice ReadFrom hands to the
// source and WriteTo to the destination lies in the engine's buffers.
func testCopyLendsEngineBuffers(t *testing.T, e engineUnderTest) {
	lending(t, e)
	client, server := e.pair(t, Config{})
	in, out := newStream(7), newStream(7)
	src := &chunkySource{s: in, rng: rand.New(rand.NewPCG(7, 0)), left: 4 << 20}
	foreignReads := make(chan int, 1)
	go func() {
		foreign := 0
		_, err := client.ReadFrom(readerFunc(func(p []byte) (int, error) {
			if !e.owns(client.s, p) {
				foreign++
			}
			return src.Read(p)
		}))
		if err != nil {
			t.Errorf("ReadFrom error: %v", err)
		}
		client.CloseWrite()
		foreignReads <- foreign
	}()
	foreignWrites := 0
	dst := &sink{s: out}
	_, err := server.WriteTo(writerFunc(func(p []byte) (int, error) {
		if !e.owns(server.s, p) {
			foreignWrites++
		}
		return dst.Write(p)
	}))
	if err != nil {
		t.Errorf("WriteTo error: %v", err)
	}
	if n := <-foreignReads; n > 0 {
		t.Errorf("ReadFrom gave the source %d buffers outside the engine's", n)
	}
	if foreignWrites > 0 {
		t.Errorf("WriteTo gave the destination %d buffers outside the engine's", foreignWrites)
	}
	if in.sum() != out.sum() {
		t.Errorf("sent %s, received %s", in.sum(), out.sum())
	}
}

// testWriteToLeavesWhatTheDestinationRefused checks that the bytes a failing
// destination did not take stay in the stream for Read.
func testWriteToLeavesWhatTheDestinationRefused(t *testing.T, e engineUnderTest) {
	lending(t, e)
	client, server := e.pair(t, Config{})
	if _, err := server.Write([]byte("hello world")); err != nil {
		t.Fatalf("Write error: %v", err)
	}
	n, err := client.WriteTo(writerFunc(func(p []byte) (int, error) { return min(len(p), 5), errDest }))
	if n != 5 || !isOpError(err, "writeto", errDest) {
		t.Fatalf("WriteTo(a writer failing after 5 bytes) = %d, %v; want 5 and a writeto *net.OpError with %v", n, err, errDest)
	}
	client.SetReadDeadline(time.Now().Add(time.Second))
	rest := make([]byte, 6)
	if _, err := io.ReadFull(client, rest); err != nil || string(rest) != " world" {
		t.Fatalf("Read after WriteTo = %q, %v; want %q", rest, err, " world")
	}
}

// testWriteToFreesRoomOnlyOnceTaken checks that the peer gets the room of
// the bytes WriteTo hands over only once the destination has taken them.
func testWriteToFreesRoomOnlyOnceTaken(t *testing.T, e engineUnderTest) {
	lending(t, e)
	client, server := e.pair(t, Config{ReceiveBufferSize: 4096})
	if _, err := server.Write(make([]byte, 4096)); err != nil {
		t.Fatalf("Write error: %v", err)
	}
	handed, release := make(chan struct{}), make(chan struct{})
	done := make(chan error, 1)
	go func() {
		once := false
		_, err := client.WriteTo(writerFunc(func(p []byte) (int, error) {
			if !once {
				once = true
				close(handed)
				<-release
			}
			return len(p), nil
		}))
		done <- err
	}()
	<-handed
	server.SetWriteDeadline(time.Now().Add(100 * time.Millisecond))
	if _, err := server.Write([]byte("x")); !errors.Is(err, os.ErrDeadlineExceeded) {
		t.Errorf("peer Write while the destination holds the bytes error = %v, want a deadline error", err)
	}
	close(release)
	server.SetWriteDeadline(time.Now().Add(5 * time.Second))
	if _, err := server.Write([]byte("x")); err != nil {
		t.Errorf("peer Write once the destination took the bytes error: %v", err)
	}
	server.CloseWrite()
	if err := <-done; err != nil {
		t.Errorf("WriteTo error: %v", err)
	}
}

// fill is a source that fills every buffer of the engine it is given, rounds
// times, and fails on any other.
type fill struct {
	rounds int
	owns   func([]byte) bool
}

var errForeign = errors.New("buffer outside the engine's")

func (f *fill) Read(p []byte) (int, error) {
	if !f.owns(p) {
		return 0, errForeign
	}
	if f.rounds == 0 {
		return 0, io.EOF
	}
	f.rounds--
	return len(p), nil
}

// testLendingRoundsDoNotAllocate checks that a round of ReadFrom or WriteTo
// through lent buffers allocates nothing: twice the rounds cost as many
// allocations.
func testLendingRoundsDoNotAllocate(t *testing.T, e engineUnderTest) {
	lending(t, e)
	const chunk = 1 << 10
	client, server := e.pair(t, Config{SendBufferSize: chunk, ReceiveBufferSize: 1 << 20})
	src := &fill{owns: func(p []byte) bool { return e.owns(client.s, p) }}
	buf := make([]byte, 1<<20)
	readFrom := func(rounds int) float64 {
		return testing.AllocsPerRun(20, func() {
			src.rounds = rounds
			if _, err := client.ReadFrom(src); err != nil {
				t.Fatalf("ReadFrom error: %v", err)
			}
			for got := 0; got < rounds*chunk; {
				n, err := server.Read(buf)
				if err != nil {
					t.Fatalf("Read error: %v", err)
				}
				got += n
			}
		})
	}
	if few, many := readFrom(64), readFrom(128); many != few {
		t.Errorf("ReadFrom of 64 and 128 rounds: %v and %v allocations; want as many", few, many)
	}

	rounds := 0
	dst := writerFunc(func(p []byte) (int, error) {
		if !e.owns(server.s, p) {
			return 0, errForeign
		}
		if rounds == 0 {
			return len(p), errStop
		}
		rounds--
		if _, err := client.Write(buf[:chunk]); err != nil {
			t.Fatalf("Write error: %v", err)
		}
		return len(p), nil
	})
	writeTo := func(n int) float64 {
		return testing.AllocsPerRun(20, func() {
			rounds = n
			if _, err := client.Write(buf[:chunk]); err != nil {
				t.Fatalf("Write error: %v", err)
			}
			if _, err := server.WriteTo(dst); !errors.Is(err, errStop) {
				t.Fatalf("WriteTo error = %v, want %v", err, errStop)
			}
		})
	}
	if few, many := writeTo(64), writeTo(128); many != few {
		t.Errorf("WriteTo of 64 and 128 rounds: %v and %v allocations; want as many", few, many)
	}
}
