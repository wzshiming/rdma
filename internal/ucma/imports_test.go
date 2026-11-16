package ucma

import (
	"go/parser"
	"go/token"
	"path/filepath"
	"strconv"
	"strings"
	"testing"
)

// The package drives the kernel device itself: no file it builds from, on
// any platform, imports a binding of the RDMA libraries.
func TestImportsNoLibraryBinding(t *testing.T) {
	names, err := filepath.Glob("*.go")
	if err != nil {
		t.Fatal(err)
	}
	forbidden := []string{
		"github.com/wzshiming/rdma/verbs",
		"github.com/wzshiming/rdma/internal/dynlib",
		"github.com/wzshiming/rdma/internal/rdmacm",
		"github.com/wzshiming/rdma/internal/rsocket",
	}
	fset := token.NewFileSet()
	checked := 0
	for _, name := range names {
		if strings.HasSuffix(name, "_test.go") {
			continue
		}
		f, err := parser.ParseFile(fset, name, nil, parser.ImportsOnly)
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
	}
	if checked == 0 {
		t.Fatal("no file of the package to check")
	}
}
