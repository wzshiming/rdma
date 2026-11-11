package rdma

import (
	"bytes"
	"errors"
	"go/ast"
	"go/parser"
	"go/token"
	"io"
	"net/netip"
	"path/filepath"
	"slices"
	"strings"
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
			if imp.Path.Value == `"github.com/wzshiming/rdma/rsocket"` {
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
