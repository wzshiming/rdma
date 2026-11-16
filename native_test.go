package rdma

import (
	"bytes"
	"errors"
	"go/ast"
	"go/parser"
	"go/token"
	"io"
	"net"
	"net/netip"
	"path/filepath"
	"runtime"
	"slices"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/wzshiming/rdma/internal/rsproto"
)

func TestNativeConnOnMemLink(t *testing.T) { testConn(t, nativeOnMemLink) }

func TestNativeCopyPathsOnMemLink(t *testing.T) { testCopyPaths(t, nativeOnMemLink) }

// Close waits for the data posted and the end of the connection to reach the
// peer, which takes them though its application does not read and then
// reads all of it and EOF; when nothing reaches the peer, Close gives up
// within a second.
func TestNativeCloseWaitsAtMostASecond(t *testing.T) {
	t.Run("peer not reading", func(t *testing.T) {
		client, server := nativeOnMemLink.pair(t, Config{})
		want := make([]byte, 64<<10)
		newStream(1).rng.Read(want)
		if _, err := client.Write(want); err != nil {
			t.Fatalf("Write error: %v", err)
		}
		start := time.Now()
		if err := client.Close(); err != nil {
			t.Fatalf("Close error: %v", err)
		}
		if elapsed := time.Since(start); elapsed >= time.Second {
			t.Errorf("Close took %v with the peer taking what arrives, want less than a second", elapsed)
		}
		got, err := io.ReadAll(server)
		if err != nil || !bytes.Equal(got, want) {
			t.Errorf("peer ReadAll after Close = %d bytes, %v; want the %d written and nil", len(got), err, len(want))
		}
	})
	t.Run("peer silent", func(t *testing.T) {
		client, server := nativeOnMemLink.pair(t, Config{})
		peer := memTransportOf(server)
		peer.silence.Lock()
		closed := make(chan struct{})
		start := time.Now()
		go func() {
			client.Close()
			close(closed)
		}()
		select {
		case <-closed:
		case <-time.After(5 * time.Second):
			peer.silence.Unlock()
			t.Fatal("Close did not return within 5 seconds with the peer silent")
		}
		elapsed := time.Since(start)
		peer.silence.Unlock()
		if elapsed > time.Second+500*time.Millisecond {
			t.Errorf("Close took %v with the peer silent, want about a second at most", elapsed)
		}
	})
}

// In steady state a Write and the Read of what it wrote allocate nothing,
// the waits for room and data included: twice the rounds cost as many
// allocations.
func TestNativeReadWriteRoundsDoNotAllocate(t *testing.T) {
	client, server := nativeOnMemLink.pair(t, Config{})
	const chunk = 1 << 10
	wbuf, rbuf := make([]byte, chunk), make([]byte, chunk)
	readWrite := func(rounds int) float64 {
		return testing.AllocsPerRun(20, func() {
			for range rounds {
				if _, err := client.Write(wbuf); err != nil {
					t.Fatalf("Write error: %v", err)
				}
				if _, err := io.ReadFull(server, rbuf); err != nil {
					t.Fatalf("ReadFull error: %v", err)
				}
			}
		})
	}
	if few, many := readWrite(64), readWrite(128); many != few {
		t.Errorf("Write and Read of 64 and 128 rounds: %v and %v allocations; want as many", few, many)
	}
}

// A Read that finds nothing to read declares that it waits for arrivals and
// parks; a message that arrives then wakes it, processed in the background.
func TestNativeArrivalWakesAParkedRead(t *testing.T) {
	client, server := nativeOnMemLink.pair(t, Config{})
	read := async(func() (int64, error) {
		n, err := server.Read(make([]byte, 1))
		return int64(n), err
	})
	armedFor(t, memTransportOf(server), arrivals, "arrivals")
	if _, err := client.Write([]byte("x")); err != nil {
		t.Fatalf("Write error: %v", err)
	}
	if r := await(t, "the parked Read", read); r.n != 1 || r.err != nil {
		t.Errorf("the parked Read = %d, %v; want 1 and nil", r.n, r.err)
	}
}

// A Write that finds the send buffer in flight declares that it waits for
// the completions of sends and parks; one that comes then wakes it.
func TestNativeSendCompletionWakesAParkedWrite(t *testing.T) {
	client, _ := nativeOnMemLink.pair(t, Config{SendBufferSize: rsproto.SndLowat})
	tr := memTransportOf(client)
	tr.holdAcks()
	const size = rsproto.SndLowat + 1
	write := async(func() (int64, error) {
		n, err := client.Write(make([]byte, size))
		return int64(n), err
	})
	armedFor(t, tr, sendCompletions, "the completions of sends")
	tr.releaseAcks()
	if r := await(t, "the parked Write", write); r.n != size || r.err != nil {
		t.Errorf("the parked Write = %d, %v; want %d and nil", r.n, r.err, size)
	}
}

// heldWindow is the receive buffer of the connections of the tests of waits
// for grants and of grants held: every half of it the peer reads earns a
// grant, which takes a control slot. heldRoom is how many halves the writer
// can write while the acknowledgements of the grants are held: two granted
// with the connection and one for each slot.
const (
	heldWindow, heldHalf = 2 << 10, 1 << 10
	heldRoom             = 2 + rsproto.QPCtrlSize
)

// A Write that has used up the room the peer granted declares that it waits
// for arrivals too: the grant that arrives as the peer reads wakes it, while
// no completion of a send comes.
func TestNativeGrantWakesAParkedWrite(t *testing.T) {
	client, server := nativeOnMemLink.pair(t, Config{ReceiveBufferSize: heldWindow})
	tr := memTransportOf(client)
	tr.holdAcks()
	defer tr.releaseAcks()
	const size = heldWindow + 1
	write := async(func() (int64, error) {
		n, err := client.Write(make([]byte, size))
		return int64(n), err
	})
	armedFor(t, tr, arrivals, "arrivals")
	if _, err := io.ReadFull(server, make([]byte, heldHalf)); err != nil {
		t.Fatalf("ReadFull error: %v", err)
	}
	if r := await(t, "the parked Write", write); r.n != size || r.err != nil {
		t.Errorf("the parked Write = %d, %v; want %d and nil", r.n, r.err, size)
	}
}

// An arrival that leaves a parked Read nothing to read, a grant, does not
// end its wait: the transport arms again after it, and the data that arrives
// next wakes the Read.
func TestNativeWaitArmsAgainUntilItEnds(t *testing.T) {
	client, server := nativeOnMemLink.pair(t, Config{ReceiveBufferSize: heldWindow})
	if _, err := server.Write(make([]byte, heldHalf)); err != nil {
		t.Fatalf("Write error: %v", err)
	}
	tr := memTransportOf(server)
	read := async(func() (int64, error) {
		n, err := server.Read(make([]byte, 1))
		return int64(n), err
	})
	armedFor(t, tr, arrivals, "arrivals")
	arms := tr.armCount()
	// Reading what the server wrote earns it a grant.
	if _, err := io.ReadFull(client, make([]byte, heldHalf)); err != nil {
		t.Fatalf("ReadFull error: %v", err)
	}
	eventually(t, "the transport of the parked Read armed again after the grant", func() bool { return tr.armCount() > arms })
	if _, err := client.Write([]byte("x")); err != nil {
		t.Fatalf("Write error: %v", err)
	}
	if r := await(t, "the parked Read", read); r.n != 1 || r.err != nil {
		t.Errorf("the parked Read = %d, %v; want 1 and nil", r.n, r.err)
	}
}

// A message that arrives after a Read last looked and before its transport
// is armed raises no event, so the transport looks again after the arm: the
// Read gets it.
func TestNativeWaitTakesWhatCameBeforeTheArm(t *testing.T) {
	client, server := nativeOnMemLink.pair(t, Config{})
	tr := memTransportOf(server)
	var werr error
	tr.beforeNextArm(func() {
		_, werr = client.Write([]byte("x"))
		tr.takeIn()
	})
	read := async(func() (int64, error) {
		n, err := server.Read(make([]byte, 1))
		return int64(n), err
	})
	if r := await(t, "the Read whose message came before the arm", read); r.n != 1 || r.err != nil {
		t.Errorf("the Read whose message came before the arm = %d, %v; want 1 and nil", r.n, r.err)
	}
	if werr != nil {
		t.Errorf("Write error: %v", werr)
	}
}

// A grant due while every control slot is in flight goes out once a slot
// comes back, though the reading end makes no call after the read that made
// it due, nor does anything arrive: the writer waiting for it gets the room.
func TestNativeHeldGrantGoesOutWithoutACall(t *testing.T) {
	client, server := nativeOnMemLink.pair(t, Config{ReceiveBufferSize: heldWindow})
	settle(t, server)
	tr := memTransportOf(server)
	tr.holdAcks()
	want := make([]byte, (heldRoom+1)*heldHalf)
	newStream(29).rng.Read(want)
	write := async(func() (int64, error) {
		n, err := client.Write(want)
		return int64(n), err
	})
	got := make([]byte, len(want))
	read := heldRoom * heldHalf
	readUntilHeld(t, server, got[:read])
	tr.releaseAcks()
	if r := await(t, "the Write waiting for the grant held", write); r.n != int64(len(want)) || r.err != nil {
		t.Fatalf("the Write waiting for the grant held = %d, %v; want %d and nil", r.n, r.err, len(want))
	}
	if _, err := io.ReadFull(server, got[read:]); err != nil || !bytes.Equal(got, want) {
		t.Errorf("ReadFull of the rest error: %v; what was read is the same as what was written: %v", err, bytes.Equal(got, want))
	}
}

// An end CloseWrite holds while every control slot is in flight goes out
// once a slot comes back, without a further call: the peer reads io.EOF.
func TestNativeHeldEndGoesOutWithoutACall(t *testing.T) {
	client, server := nativeOnMemLink.pair(t, Config{ReceiveBufferSize: heldWindow})
	settle(t, server)
	tr := memTransportOf(server)
	tr.holdAcks()
	size := rsproto.QPCtrlSize * heldHalf
	write := async(func() (int64, error) {
		n, err := client.Write(make([]byte, size))
		return int64(n), err
	})
	readHalves(t, server, make([]byte, size))
	if err := server.CloseWrite(); err != nil {
		t.Fatalf("CloseWrite error: %v", err)
	}
	tr.releaseAcks()
	if r := await(t, "Write", write); r.n != int64(size) || r.err != nil {
		t.Fatalf("Write = %d, %v; want %d and nil", r.n, r.err, size)
	}
	read := async(func() (int64, error) {
		n, err := client.Read(make([]byte, 1))
		return int64(n), err
	})
	if r := await(t, "the Read of the end held", read); r.n != 0 || r.err != io.EOF {
		t.Errorf("the Read of the end held = %d, %v; want 0 and io.EOF", r.n, r.err)
	}
}

// A grant and an end both held for slots go out one per slot as the
// completions come one at a time: the transport arms again after each, as
// long as something is held.
func TestNativeHeldControlArmsAgainUntilItIsOut(t *testing.T) {
	client, server := nativeOnMemLink.pair(t, Config{ReceiveBufferSize: heldWindow})
	settle(t, server)
	tr := memTransportOf(server)
	tr.holdAcks()
	defer tr.releaseAcks()
	size := (heldRoom + 1) * heldHalf
	write := async(func() (int64, error) {
		n, err := client.Write(make([]byte, size))
		return int64(n), err
	})
	readUntilHeld(t, server, make([]byte, (rsproto.QPCtrlSize+1)*heldHalf))
	if err := server.CloseWrite(); err != nil {
		t.Fatalf("CloseWrite error: %v", err)
	}
	arms := tr.armCount()
	tr.ack(1) // the end goes out, the grant waits for the next slot
	eventually(t, "the transport armed again for the grant still held", func() bool { return tr.armCount() > arms })
	tr.ack(1)
	if r := await(t, "the Write waiting for the grant held", write); r.n != int64(size) || r.err != nil {
		t.Errorf("the Write waiting for the grant held = %d, %v; want %d and nil", r.n, r.err, size)
	}
}

// readHalves reads p from c a half of heldWindow at a time, each read
// earning its own grant.
func readHalves(t *testing.T, c *Conn, p []byte) {
	t.Helper()
	for ; len(p) > 0; p = p[heldHalf:] {
		if _, err := io.ReadFull(c, p[:heldHalf]); err != nil {
			t.Fatalf("ReadFull error: %v", err)
		}
	}
}

// readUntilHeld reads p from c, a half at a time, while the acknowledgements
// of its grants are held: QPCtrlSize halves, each of whose grants takes a
// slot, and, once the peer has used the room they give and nothing more can
// arrive, the rest, whose grants are held.
func readUntilHeld(t *testing.T, c *Conn, p []byte) {
	t.Helper()
	inFlight := rsproto.QPCtrlSize * heldHalf
	readHalves(t, c, p[:inFlight])
	s := c.s.(*nativeStream)
	eventually(t, "the room of the grants in flight used", func() bool {
		s.t.process()
		s.mu.Lock()
		defer s.mu.Unlock()
		return len(s.st.Peek()) == heldWindow
	})
	readHalves(t, c, p[inFlight:])
}

// Close declares that it waits for the completions of the sends posted,
// which the transport processes in the background as they come, one at a
// time here, arming again after each, and returns once the last has come,
// well within closeTimeout.
func TestNativeCloseDrainsTheSendsInTheBackground(t *testing.T) {
	client, server := nativeOnMemLink.pair(t, Config{})
	settle(t, client)
	tr := memTransportOf(client)
	tr.holdAcks()
	if _, err := client.Write([]byte("hello")); err != nil {
		t.Fatalf("Write error: %v", err)
	}
	start := time.Now()
	closed := async(func() (int64, error) { return 0, client.Close() })
	armedFor(t, tr, sendCompletions, "the completions of sends")
	arms := tr.armCount()
	tr.ack(1) // the data; the end of the connection is still in flight
	eventually(t, "the transport of the closing end armed again", func() bool { return tr.armCount() > arms })
	tr.releaseAcks()
	if r := await(t, "Close", closed); r.err != nil {
		t.Fatalf("Close error: %v", r.err)
	}
	if elapsed := time.Since(start); elapsed >= closeTimeout {
		t.Errorf("Close took %v, want it to return as the last completion came", elapsed)
	}
	if got, err := io.ReadAll(server); string(got) != "hello" || err != nil {
		t.Errorf("peer ReadAll after Close = %q, %v; want %q and nil", got, err, "hello")
	}
}

// Goroutines that have the transports process at once, besides those that
// read and write, leave every completion handed to the stream once and in
// order: what arrives is what was written.
func TestNativeConcurrentProcessingKeepsTheOrder(t *testing.T) {
	client, server := nativeOnMemLink.pair(t, Config{SendBufferSize: 8 << 10, ReceiveBufferSize: 8 << 10, SendQueueSize: 16, ReceiveQueueSize: 16})
	stop := make(chan struct{})
	var wg sync.WaitGroup
	for _, c := range []*Conn{client, server} {
		tr := memTransportOf(c)
		for i := range 4 {
			wg.Go(func() {
				for {
					select {
					case <-stop:
						return
					default:
					}
					if i%2 == 0 {
						tr.process()
					} else {
						tr.expect(anyCompletion)
					}
					runtime.Gosched()
				}
			})
		}
	}
	transfer(t, client, server, 256<<10, 1000, client.CloseWrite)
	close(stop)
	wg.Wait()
}

// Closes that race Reads and Writes waiting at both ends, again and again,
// leave nothing drained or armed once the memory is released, which the
// memory engine checks.
func TestNativeCloseRacingWaits(t *testing.T) {
	for range 50 {
		client, server := nativeOnMemLink.pair(t, Config{})
		var wg sync.WaitGroup
		for _, c := range []*Conn{client, server} {
			wg.Go(func() {
				buf := make([]byte, 1<<10)
				for {
					if _, err := c.Read(buf); err != nil {
						return
					}
				}
			})
			wg.Go(func() {
				buf := make([]byte, 64<<10)
				for {
					if _, err := c.Write(buf); err != nil {
						return
					}
				}
			})
		}
		wg.Go(func() { client.Close() })
		server.Close()
		wg.Wait()
	}
}

// A wait that comes after Close, as one of a Read can, drains and arms
// nothing: the memory of the stream is released.
func TestNativeWaitAfterCloseProcessesNothing(t *testing.T) {
	client, _ := nativeOnMemLink.pair(t, Config{})
	if err := client.Close(); err != nil {
		t.Fatalf("Close error: %v", err)
	}
	tr := memTransportOf(client)
	for _, dir := range []direction{readable, writable} {
		if err := client.s.wait(dir, nil); !errors.Is(err, net.ErrClosed) {
			t.Errorf("wait(%d) after Close error = %v, want net.ErrClosed", dir, err)
		}
	}
	tr.expect(anyCompletion)
	if misuse := tr.misuseSeen(); misuse != "" {
		t.Errorf("after Close the transport was %s", misuse)
	}
}

// armedFor waits until the queues of tr are armed for the kinds k, what,
// failing t after 5 seconds.
func armedFor(t *testing.T, tr *memTransport, k interest, what string) {
	t.Helper()
	eventually(t, "the transport armed for "+what, func() bool { return tr.isArmed(k) })
}

// eventually polls cond until it holds, failing t after 5 seconds.
func eventually(t *testing.T, what string, cond func() bool) {
	t.Helper()
	for deadline := time.Now().Add(5 * time.Second); !cond(); time.Sleep(time.Millisecond) {
		if time.Now().After(deadline) {
			t.Fatalf("%s: not within 5 seconds", what)
		}
	}
}

// settle has the transport of c, a connection of the memory engine,
// process until every send of c has completed.
func settle(t *testing.T, c *Conn) {
	t.Helper()
	s := c.s.(*nativeStream)
	eventually(t, "the sends completed", func() bool {
		s.t.process()
		s.mu.Lock()
		defer s.mu.Unlock()
		return s.sendsDone()
	})
}

// A link lost without the peer's end of the connection reads as ECONNRESET
// once the data that arrived before is read.
func TestNativeLostLinkReadsAsResetAfterTheData(t *testing.T) {
	client, server := nativeOnMemLink.pair(t, Config{})
	if _, err := client.Write([]byte("hello")); err != nil {
		t.Fatalf("Write error: %v", err)
	}
	if _, err := io.ReadFull(server, make([]byte, 1)); err != nil {
		t.Fatalf("ReadFull error: %v", err)
	}
	memTransportOf(client).breakLink(errors.New("cable cut"))
	rest, err := io.ReadAll(server)
	if string(rest) != "ello" || !isOpError(err, "read", errConnReset) {
		t.Errorf("peer read %q, %v after the link was lost; want %q and a read *net.OpError with ECONNRESET", rest, err, "ello")
	}
}

// A send the transport refuses breaks the stream, and the Write that made it
// fails with ECONNRESET.
func TestNativeRefusedSendFailsTheWrite(t *testing.T) {
	client, _ := nativeOnMemLink.pair(t, Config{})
	tr := memTransportOf(client)
	// Silent, the pump posts no receive that would take the failure.
	tr.silence.Lock()
	defer tr.silence.Unlock()
	tr.end.FailPost(errors.New("send refused"))
	if n, err := client.Write([]byte("hello")); !isOpError(err, "write", errConnReset) {
		t.Errorf("Write with its send refused = %d, %v; want a write *net.OpError with ECONNRESET", n, err)
	}
}

// A view lent before Close stays usable after it, and the memory of the
// stream is released only once every view is back.
func TestNativeViewsOutliveClose(t *testing.T) {
	client, server := nativeOnMemLink.pair(t, Config{})
	if _, err := server.Write([]byte("hello")); err != nil {
		t.Fatalf("Write error: %v", err)
	}
	l := client.s.(lender)
	rview, err := l.lendRead()
	for errors.Is(err, errAgain) {
		client.s.wait(readable, nil)
		rview, err = l.lendRead()
	}
	if err != nil {
		t.Fatalf("lendRead error: %v", err)
	}
	wview, err := l.lendWrite()
	if err != nil {
		t.Fatalf("lendWrite error: %v", err)
	}
	tr := memTransportOf(client)
	if err := client.Close(); err != nil {
		t.Fatalf("Close error: %v", err)
	}
	copy(wview, "written after Close")
	if string(rview) != "hello" || tr.isReleased() {
		t.Errorf("after Close the read view holds %q and the memory is released: %v; want %q and false", rview, tr.isReleased(), "hello")
	}
	if err := l.returnRead(len(rview)); !errors.Is(err, net.ErrClosed) {
		t.Errorf("returnRead after Close error = %v, want net.ErrClosed", err)
	}
	if tr.isReleased() {
		t.Error("memory released with the write view still out")
	}
	if err := l.commitWrite(len(wview)); !errors.Is(err, net.ErrClosed) {
		t.Errorf("commitWrite after Close error = %v, want net.ErrClosed", err)
	}
	if !tr.isReleased() {
		t.Error("memory not released once both views are back")
	}
}

// A receive the transport refuses fails the building of the stream, before
// any connection data goes out.
func TestNativeStreamFailsOnARefusedReceive(t *testing.T) {
	end, _ := rsproto.NewMemLink()
	refused := errors.New("receive refused")
	end.FailPost(refused)
	tr := newMemTransport(end, memConfig(Config{}))
	s, err := newNativeStream(tr, tr.mem, netip.AddrPort{}, netip.AddrPort{})
	if s != nil || !errors.Is(err, refused) {
		t.Errorf("newNativeStream on a transport refusing a receive = %v, %v; want no stream and an error wrapping %q", s, err, refused)
	}
}

// The native engine's files import no rsocket and leave the message words,
// credits and grants of rsockets to internal/rsproto: they name none of its
// operations, control codes and helpers of message words and grant entries,
// and take no message word apart.
func TestNativeEngineLeavesTheProtocolToRsproto(t *testing.T) {
	names, err := filepath.Glob("native*.go")
	if err != nil {
		t.Fatal(err)
	}
	if !slices.ContainsFunc(names, func(name string) bool { return !strings.HasSuffix(name, "_test.go") }) {
		t.Fatalf("no native engine file among %q", names)
	}
	fset := token.NewFileSet()
	for _, name := range names {
		f, err := parser.ParseFile(fset, name, nil, parser.SkipObjectResolution)
		if err != nil {
			t.Fatal(err)
		}
		for _, imp := range f.Imports {
			if imp.Path.Value == `"github.com/wzshiming/rdma/internal/rsocket"` {
				t.Errorf("%s imports rsocket", name)
			}
		}
		ast.Inspect(f, func(n ast.Node) bool {
			switch n := n.(type) {
			case *ast.SelectorExpr:
				if x, ok := n.X.(*ast.Ident); ok && x.Name == "rsproto" && isProtocolArithmetic(n.Sel.Name) {
					t.Errorf("%s: rsproto.%s", fset.Position(n.Pos()), n.Sel.Name)
				}
			case *ast.CallExpr:
				if sel, ok := n.Fun.(*ast.SelectorExpr); ok && len(n.Args) == 0 && (sel.Sel.Name == "Op" || sel.Sel.Name == "Data") {
					t.Errorf("%s: a message word taken apart with %s", fset.Position(n.Pos()), sel.Sel.Name)
				}
			}
			return true
		})
	}
}

// isProtocolArithmetic reports whether name is one of rsproto's operations,
// control codes and helpers of message words and grant entries.
func isProtocolArithmetic(name string) bool {
	switch name {
	case "MakeMsg", "PutSGE", "GetSGE":
		return true
	}
	return strings.HasPrefix(name, "Op") || strings.HasPrefix(name, "Ctrl")
}
