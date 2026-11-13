package rdma

import (
	"go/ast"
	"go/parser"
	"go/token"
	"path/filepath"
	"regexp"
	"strings"
	"testing"
)

// engineName names an engine the tests run on: the native engine, which
// Dial and Listen run on, or the librdmacm engine, its control.
type engineName string

const (
	engineNative    engineName = "native"
	engineLibrdmacm engineName = "librdmacm"
)

// Dial and Listen run on the native engine.
func TestPlatformEngineIsNative(t *testing.T) {
	got := platformEngine()
	if _, ok := got.(nativeEngine); !ok {
		t.Errorf("platformEngine() is a %T, want a nativeEngine", got)
	}
}

// The binding of librdmacm's rsockets serves the tests alone, whose control
// the librdmacm engine is: no file the package builds from, on any platform,
// imports it.
func TestOnlyTheTestsImportTheBinding(t *testing.T) {
	names, err := filepath.Glob("*.go")
	if err != nil {
		t.Fatal(err)
	}
	fset := token.NewFileSet()
	for _, name := range names {
		if strings.HasSuffix(name, "_test.go") {
			continue
		}
		f, err := parser.ParseFile(fset, name, nil, parser.ImportsOnly)
		if err != nil {
			t.Fatal(err)
		}
		for _, imp := range f.Imports {
			if imp.Path.Value == `"github.com/wzshiming/rdma/internal/rsocket"` {
				t.Errorf("%s imports internal/rsocket", name)
			}
		}
	}
}

// The package offers no engine, so no text of it names one: no string
// literal of the files it builds from, on any platform, the texts of its
// errors among them.
func TestTextsNameNoEngine(t *testing.T) {
	names, err := filepath.Glob("*.go")
	if err != nil {
		t.Fatal(err)
	}
	engine := regexp.MustCompile(`(?i)\bengines?\b`)
	fset := token.NewFileSet()
	for _, name := range names {
		if strings.HasSuffix(name, "_test.go") {
			continue
		}
		f, err := parser.ParseFile(fset, name, nil, parser.SkipObjectResolution)
		if err != nil {
			t.Fatal(err)
		}
		ast.Inspect(f, func(n ast.Node) bool {
			if lit, ok := n.(*ast.BasicLit); ok && lit.Kind == token.STRING && engine.MatchString(lit.Value) {
				t.Errorf("%s: %s names an engine", fset.Position(lit.Pos()), lit.Value)
			}
			return true
		})
	}
}

// The front end leaves the engine to the platform entry and to the tests'
// injection: its files neither build, call, convert to, assert nor switch on
// anything the files of an engine declare.
func TestFrontEndHasNoEngineBranch(t *testing.T) {
	engineFiles, err := filepath.Glob("native*.go")
	if err != nil {
		t.Fatal(err)
	}
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
			case *ast.SelectorExpr:
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
