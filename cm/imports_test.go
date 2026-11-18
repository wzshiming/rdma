package cm

import (
	"go/ast"
	"go/parser"
	"go/token"
	"path/filepath"
	"regexp"
	"strconv"
	"strings"
	"testing"
)

// The package is the connection manager itself, on the kernel's device and
// on verbs: no file it builds from, on any platform, imports the binding of
// librdmacm, that of rsockets or the dynamic loader, or names a shared
// library.
func TestImportsNoLibraryBinding(t *testing.T) {
	names, err := filepath.Glob("*.go")
	if err != nil {
		t.Fatal(err)
	}
	forbidden := []string{
		"github.com/wzshiming/rdma/internal/dynlib",
		"github.com/wzshiming/rdma/internal/rdmacm",
		"github.com/wzshiming/rdma/internal/rsocket",
	}
	library := regexp.MustCompile(`\.so\b`)
	fset := token.NewFileSet()
	checked := 0
	for _, name := range names {
		if strings.HasSuffix(name, "_test.go") {
			continue
		}
		f, err := parser.ParseFile(fset, name, nil, 0)
		if err != nil {
			t.Fatal(err)
		}
		checked++
		for _, imp := range f.Imports {
			path, err := strconv.Unquote(imp.Path.Value)
			if err != nil {
				t.Fatal(err)
			}
			for _, p := range forbidden {
				if path == p {
					t.Errorf("%s imports %s", name, p)
				}
			}
		}
		ast.Inspect(f, func(n ast.Node) bool {
			lit, ok := n.(*ast.BasicLit)
			if !ok || lit.Kind != token.STRING {
				return true
			}
			if s, err := strconv.Unquote(lit.Value); err == nil && library.MatchString(s) {
				t.Errorf("%s names the shared library %q", fset.Position(lit.Pos()), s)
			}
			return true
		})
	}
	if checked == 0 {
		t.Fatal("no file of the package to check")
	}
}
