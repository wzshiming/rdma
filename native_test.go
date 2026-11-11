package rdma

import (
	"errors"
	"go/ast"
	"go/parser"
	"go/token"
	"net/netip"
	"path/filepath"
	"slices"
	"strings"
	"testing"

	"github.com/wzshiming/rdma/internal/rsproto"
)

func TestNativeConnOnMemLink(t *testing.T) { testConn(t, nativeOnMemLink) }

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
