package rdma

import (
	"context"
	"errors"
	"go/ast"
	"go/parser"
	"go/token"
	"path/filepath"
	"reflect"
	"strings"
	"testing"
)

// Config.Engine selects the engine behind the platform entry: the native
// engine for the zero value and EngineNative, the librdmacm engine for
// EngineLibrdmacm. A value that names no engine fails Dial and Listen with an
// error about the Config, which names it, before any engine is asked.
func TestConfigEngineSelectsTheEngine(t *testing.T) {
	for _, tt := range []struct {
		name   string
		engine Engine
		want   engine // nil when refused
	}{
		{"zero value", "", nativeEngine{}},
		{"EngineNative", EngineNative, nativeEngine{}},
		{"EngineLibrdmacm", EngineLibrdmacm, librdmacmEngine{}},
		{"unknown", "rsocket", nil},
	} {
		t.Run(tt.name, func(t *testing.T) {
			c := Config{Engine: tt.engine}
			if tt.want != nil {
				if err := c.validate(); err != nil {
					t.Fatalf("Config{Engine: %q}.validate() = %v, want nil", tt.engine, err)
				}
				if got := platformEngine(c); reflect.TypeOf(got) != reflect.TypeOf(tt.want) {
					t.Errorf("platformEngine(Config{Engine: %q}) is a %T, want a %T", tt.engine, got, tt.want)
				}
				return
			}
			d := Dialer{Config: c, eng: untouchedEngine{t}}
			if _, err := d.Dial("rdma", "10.99.0.1:1"); !isOpError(err, "dial", errConfig) || !strings.Contains(err.Error(), `"rsocket"`) {
				t.Errorf("Dial() with Config{Engine: %q} error = %v, want a dial *net.OpError matching errConfig that names the value", tt.engine, err)
			}
			lc := ListenConfig{Config: c, eng: untouchedEngine{t}}
			if _, err := lc.Listen(t.Context(), "rdma", "10.99.0.1:0"); !isOpError(err, "listen", errConfig) || !strings.Contains(err.Error(), `"rsocket"`) {
				t.Errorf("Listen() with Config{Engine: %q} error = %v, want a listen *net.OpError matching errConfig that names the value", tt.engine, err)
			}
		})
	}
}

// untouchedEngine fails the test when it is asked to dial or listen.
type untouchedEngine struct{ t *testing.T }

func (e untouchedEngine) dial(context.Context, *Addr, *Addr, Config) (stream, error) {
	e.t.Error("the engine was asked to dial")
	return nil, errors.ErrUnsupported
}

func (e untouchedEngine) listen(*Addr, int, Config) (acceptor, error) {
	e.t.Error("the engine was asked to listen")
	return nil, errors.ErrUnsupported
}

// The front end treats the engines alike and leaves the choice to the
// platform entry: its files neither read Config.Engine nor name an Engine
// value, and neither build, call, convert to, assert nor switch on anything
// the files of an engine declare.
func TestFrontEndHasNoEngineBranch(t *testing.T) {
	engineFiles, err := filepath.Glob("native*.go")
	if err != nil {
		t.Fatal(err)
	}
	engineFiles = append(engineFiles, "network.go", "network_stub.go", "poll_linux.go")
	fset := token.NewFileSet()
	parse := func(name string) *ast.File {
		f, err := parser.ParseFile(fset, name, nil, parser.SkipObjectResolution)
		if err != nil {
			t.Fatal(err)
		}
		return f
	}
	declared := map[string]bool{}
	for _, name := range engineFiles {
		if strings.HasSuffix(name, "_test.go") {
			continue
		}
		for _, d := range parse(name).Decls {
			switch d := d.(type) {
			case *ast.FuncDecl:
				if d.Recv == nil {
					declared[d.Name.Name] = true
				}
			case *ast.GenDecl:
				for _, s := range d.Specs {
					switch s := s.(type) {
					case *ast.TypeSpec:
						declared[s.Name.Name] = true
					case *ast.ValueSpec:
						for _, n := range s.Names {
							declared[n.Name] = true
						}
					}
				}
			}
		}
	}
	delete(declared, "platformEngine")
	for _, name := range []string{"conn.go", "copy.go", "dial.go", "engine.go", "listen.go"} {
		f := parse(name)
		check := func(exprs ...ast.Expr) {
			for _, e := range exprs {
				for {
					if s, ok := e.(*ast.StarExpr); ok {
						e = s.X
					} else if p, ok := e.(*ast.ParenExpr); ok {
						e = p.X
					} else {
						break
					}
				}
				if id, ok := e.(*ast.Ident); ok && declared[id.Name] {
					t.Errorf("%s: %s, which an engine declares", fset.Position(id.Pos()), id.Name)
				}
			}
		}
		ast.Inspect(f, func(n ast.Node) bool {
			switch n := n.(type) {
			case *ast.Ident:
				if n.Name == "EngineNative" || n.Name == "EngineLibrdmacm" {
					t.Errorf("%s: %s", fset.Position(n.Pos()), n.Name)
				}
			case *ast.SelectorExpr:
				if n.Sel.Name == "Engine" {
					t.Errorf("%s: Config.Engine read", fset.Position(n.Pos()))
				}
				check(n.X)
			case *ast.CallExpr:
				check(n.Fun)
			case *ast.CompositeLit:
				check(n.Type)
			case *ast.TypeAssertExpr:
				check(n.Type)
			case *ast.CaseClause:
				check(n.List...)
			}
			return true
		})
	}
}
