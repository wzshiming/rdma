package rsproto_test

import (
	"go/ast"
	"go/parser"
	"go/token"
	"os"
	"strconv"
	"strings"
	"testing"
)

// sources parses the files the package is built from, its tests left out.
func sources(t *testing.T) (*token.FileSet, []*ast.File) {
	t.Helper()
	entries, err := os.ReadDir(".")
	if err != nil {
		t.Fatal(err)
	}
	fset := token.NewFileSet()
	var files []*ast.File
	for _, e := range entries {
		if name := e.Name(); strings.HasSuffix(name, ".go") && !strings.HasSuffix(name, "_test.go") {
			f, err := parser.ParseFile(fset, name, nil, parser.SkipObjectResolution)
			if err != nil {
				t.Fatal(err)
			}
			files = append(files, f)
		}
	}
	if len(files) == 0 {
		t.Fatal("no source files")
	}
	return fset, files
}

// The package needs the standard library alone: no file imports cgo's "C" or
// a path whose first element holds a dot, by which the go command tells a
// package outside the standard library.
func TestPackageImportsOnlyTheStandardLibrary(t *testing.T) {
	fset, files := sources(t)
	for _, f := range files {
		for _, imp := range f.Imports {
			path, err := strconv.Unquote(imp.Path.Value)
			if err != nil {
				t.Fatal(err)
			}
			if first, _, _ := strings.Cut(path, "/"); path == "C" || strings.Contains(first, ".") {
				t.Errorf("%s imports %q, which is not in the standard library", fset.Position(imp.Pos()), path)
			}
		}
	}
}
