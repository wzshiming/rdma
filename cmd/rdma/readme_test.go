package main

import (
	"go/ast"
	"go/parser"
	"go/token"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"testing"
)

// repoRoot is the module root, where README.md and the root package live.
const repoRoot = "../.."

func readREADME(t *testing.T) string {
	t.Helper()
	b, err := os.ReadFile(filepath.Join(repoRoot, "README.md"))
	if err != nil {
		t.Fatal(err)
	}
	return string(b)
}

func TestREADMESections(t *testing.T) {
	want := []string{"# rdma", "## Requirements", "## Install", "## Usage", "## Packages", "## Command line", "## Soft-RoCE", "## Platforms", "## License"}
	var got []string
	inCode := false
	for _, line := range strings.Split(readREADME(t), "\n") {
		switch {
		case strings.HasPrefix(line, "```"):
			inCode = !inCode
		case !inCode && strings.HasPrefix(line, "#"):
			got = append(got, line)
		}
	}
	if !slices.Equal(got, want) {
		t.Errorf("README headings = %q, want %q", got, want)
	}
}

func TestREADMEBadges(t *testing.T) {
	readme := readREADME(t)
	for _, url := range []string{
		"https://pkg.go.dev/github.com/wzshiming/rdma",
		"https://github.com/wzshiming/rdma/actions/workflows/go.yml/badge.svg",
	} {
		if !strings.Contains(readme, url) {
			t.Errorf("README lacks the badge URL %s", url)
		}
	}
}

// TestREADMEUsageMatchesExamples checks that the Go snippets under "## Usage"
// are the bodies of the root package's ExampleDial and ExampleListen.
func TestREADMEUsageMatchesExamples(t *testing.T) {
	blocks := codeBlocks(section(readREADME(t), "## Usage"), "go")
	examples := []string{"ExampleDial", "ExampleListen"}
	if len(blocks) != len(examples) {
		t.Fatalf("the Usage section has %d Go code blocks, want %d: %v", len(blocks), len(examples), examples)
	}
	for i, name := range examples {
		if want := exampleBody(t, name); blocks[i] != want {
			t.Errorf("Usage code block %d differs from the body of %s:\n%s\nwant\n%s", i+1, name, blocks[i], want)
		}
	}
}

// section returns the text from the heading to the next heading of the
// same level.
func section(doc, heading string) string {
	_, rest, ok := strings.Cut(doc, "\n"+heading+"\n")
	if !ok {
		return ""
	}
	level := heading[:strings.Index(heading, " ")+1]
	if i := strings.Index(rest, "\n"+level); i >= 0 {
		rest = rest[:i]
	}
	return rest
}

// codeBlocks returns the contents of the fenced code blocks of a language,
// each ending with a newline.
func codeBlocks(doc, lang string) []string {
	var blocks []string
	var block []string
	inCode := false
	for _, line := range strings.Split(doc, "\n") {
		switch {
		case line == "```"+lang:
			inCode = true
			block = nil
		case inCode && line == "```":
			inCode = false
			blocks = append(blocks, strings.Join(block, "\n")+"\n")
		case inCode:
			block = append(block, line)
		}
	}
	return blocks
}

// exampleBody returns the source of an example function's body in the root
// package's example_test.go, with the function's indentation removed.
func exampleBody(t *testing.T, name string) string {
	t.Helper()
	path := filepath.Join(repoRoot, "example_test.go")
	src, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	fset := token.NewFileSet()
	f, err := parser.ParseFile(fset, path, src, parser.ParseComments)
	if err != nil {
		t.Fatal(err)
	}
	for _, d := range f.Decls {
		fn, ok := d.(*ast.FuncDecl)
		if !ok || fn.Name.Name != name {
			continue
		}
		body := string(src[fset.Position(fn.Body.Lbrace).Offset+1 : fset.Position(fn.Body.Rbrace).Offset])
		lines := strings.Split(strings.TrimPrefix(body, "\n"), "\n")
		for i, line := range lines {
			lines[i] = strings.TrimPrefix(line, "\t")
		}
		return strings.Join(lines, "\n")
	}
	t.Fatalf("%s has no %s", path, name)
	return ""
}
