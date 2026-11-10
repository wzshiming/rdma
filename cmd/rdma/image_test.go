package main

import (
	"go/parser"
	"go/token"
	"os"
	"path/filepath"
	"slices"
	"strconv"
	"strings"
	"testing"
)

const modulePath = "github.com/wzshiming/rdma"

// TestDockerignoreAdmitsEveryPackageOfTheCommand checks that the allowlist in
// .dockerignore lets every package the command is built from into the image
// build context: a package left out breaks the image build, not the tests.
func TestDockerignoreAdmitsEveryPackageOfTheCommand(t *testing.T) {
	admitted := dockerignoreAllowlist(t)
	for _, dir := range modulePackages(t, "cmd/rdma") {
		top := dir
		if i := strings.Index(dir, "/"); i >= 0 {
			top = dir[:i]
		}
		if dir == "." {
			top = "*.go"
		}
		if !slices.Contains(admitted, top) {
			t.Errorf(".dockerignore does not admit %q, which the command imports as %s/%s", top, modulePath, dir)
		}
	}
}

// dockerignoreAllowlist returns the patterns the allowlist in .dockerignore
// re-includes with "!".
func dockerignoreAllowlist(t *testing.T) []string {
	t.Helper()
	b, err := os.ReadFile(filepath.Join(repoRoot, ".dockerignore"))
	if err != nil {
		t.Fatal(err)
	}
	var allowed []string
	for _, line := range strings.Split(string(b), "\n") {
		if p, ok := strings.CutPrefix(strings.TrimSpace(line), "!"); ok {
			allowed = append(allowed, p)
		}
	}
	return allowed
}

// modulePackages returns the directories, relative to the module root, of
// the packages of this module that the package in dir depends on, itself
// included, following the imports of every non-test file.
func modulePackages(t *testing.T, dir string) []string {
	t.Helper()
	seen := map[string]bool{}
	var visit func(dir string)
	visit = func(dir string) {
		if seen[dir] {
			return
		}
		seen[dir] = true
		fset := token.NewFileSet()
		pkgs, err := parser.ParseDir(fset, filepath.Join(repoRoot, dir), func(fi os.FileInfo) bool {
			return !strings.HasSuffix(fi.Name(), "_test.go")
		}, parser.ImportsOnly)
		if err != nil {
			t.Fatalf("parse %s: %v", dir, err)
		}
		for _, pkg := range pkgs {
			for _, f := range pkg.Files {
				for _, imp := range f.Imports {
					path, err := strconv.Unquote(imp.Path.Value)
					if err != nil {
						t.Fatal(err)
					}
					if path == modulePath {
						visit(".")
					} else if rest, ok := strings.CutPrefix(path, modulePath+"/"); ok {
						visit(rest)
					}
				}
			}
		}
	}
	visit(dir)
	dirs := make([]string, 0, len(seen))
	for d := range seen {
		dirs = append(dirs, d)
	}
	slices.Sort(dirs)
	return dirs
}
