package main

import (
	"errors"
	"go/ast"
	"go/parser"
	"go/token"
	"os"
	"path/filepath"
	"regexp"
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
	want := []string{"# rdma", "## Requirements", "## Install", "## Usage", "## Packages", "## Command line", "## Docker", "## Soft-RoCE", "## Benchmarks", "## Platforms", "## License"}
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

// TestREADMEDocker checks that the Docker section names the published image
// and shows how to run and build it.
func TestREADMEDocker(t *testing.T) {
	docker := section(readREADME(t), "## Docker")
	if docker == "" {
		t.Fatal("README has no Docker section")
	}
	for _, want := range []string{
		"ghcr.io/wzshiming/rdma",
		"docker run --rm --network host --device /dev/infiniband --ulimit memlock=-1:-1 ghcr.io/wzshiming/rdma devices",
		"docker build -t rdma .",
	} {
		if !strings.Contains(docker, want) {
			t.Errorf("the Docker section lacks %q", want)
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

// TestREADMEDescribesTheRsocketsStream checks that the README names the
// rsockets knobs and their flags, and no longer the credit protocol's.
func TestREADMEDescribesTheRsocketsStream(t *testing.T) {
	readme := readREADME(t)
	for _, stale := range []string{"MessageSize", "ReceiveDepth", "SendDepth", "msgsize", "-depth"} {
		if strings.Contains(readme, stale) {
			t.Errorf("README still mentions %q", stale)
		}
	}
	for _, want := range []string{
		"rsockets", "rstream", "librspreload", "/etc/rdma/rsocket",
		"https://pkg.go.dev/github.com/wzshiming/rdma/rsocket",
		"`-sndbuf`", "`-rcvbuf`", "`-sqsize`", "`-rqsize`", "`-inline`",
		"`Config.SendBufferSize`", "`Config.ReceiveBufferSize`", "`Config.SendQueueSize`", "`Config.ReceiveQueueSize`", "`Config.MaxInline`",
	} {
		if !strings.Contains(readme, want) {
			t.Errorf("README lacks %q", want)
		}
	}
}

// TestREADMEDescribesTheEngines checks that the introduction presents the
// native engine as the default and the librdmacm engine as the choice, both
// on the rsockets protocol, that the command line names the flag of the
// choice, and that no sentence still makes the connections librdmacm's
// rsockets.
func TestREADMEDescribesTheEngines(t *testing.T) {
	readme := readREADME(t)
	intro, _, _ := strings.Cut(readme, "\n## ")
	for _, want := range []string{"rsockets protocol", "native engine, the default", "librdmacm engine", "`Config.Engine`"} {
		if !strings.Contains(intro, want) {
			t.Errorf("the introduction lacks %q", want)
		}
	}
	for _, stale := range []string{"backed by rsockets", "the stream sockets of librdmacm", "the stream above is built on"} {
		if strings.Contains(readme, stale) {
			t.Errorf("README still makes the connections librdmacm's rsockets: %q", stale)
		}
	}
	for _, s := range sentencesWith(readme, "/etc/rdma/rsocket") {
		if !strings.Contains(s, "librdmacm engine") {
			t.Errorf("README applies /etc/rdma/rsocket to every engine: %q", s)
		}
	}
	commandLine := section(readme, "## Command line")
	for _, want := range []string{"`-engine`", "`native`", "`librdmacm`", "`Config.Engine`"} {
		if !strings.Contains(commandLine, want) {
			t.Errorf("the Command line section lacks %q", want)
		}
	}
}

// TestREADMEDescribesThePoll checks that the Command line section names the
// flag of the poll before a wait, its Config field and the form that polls
// as rsockets do, for their polling_time, and no longer a negative form.
func TestREADMEDescribesThePoll(t *testing.T) {
	commandLine := section(readREADME(t), "## Command line")
	for _, want := range []string{"`-poll`", "`Config.PollTime`", "`-poll 10us`", "`polling_time`"} {
		if !strings.Contains(commandLine, want) {
			t.Errorf("the Command line section lacks %q", want)
		}
	}
	if strings.Contains(commandLine, "`-poll=-1ns`") {
		t.Error("the Command line section still turns polling off with `-poll=-1ns`")
	}
}

// TestREADMEExplainsTheCopy checks that Usage tells what io.Copy saves in
// each direction and when it does: on the native engine, for a connection
// io.Copy sees as such.
func TestREADMEExplainsTheCopy(t *testing.T) {
	usage := section(readREADME(t), "## Usage")
	for _, want := range []string{
		"`io.ReaderFrom`", "`io.WriterTo`",
		"`io.Copy(conn, src)`", "registered send buffer",
		"`io.Copy(dst, conn)`", "registered receive buffer",
		"native engine", "librdmacm engine", "wrapper",
	} {
		if !strings.Contains(usage, want) {
			t.Errorf("the Usage section lacks %q", want)
		}
	}
}

// TestREADMERequirements checks that Requirements asks for the rdma-core
// libraries at run time, no longer for the development packages and C
// compiler a cgo build needed, and confines the caveats of librdmacm's
// rsockets, the release and the configuration file, to the librdmacm engine.
func TestREADMERequirements(t *testing.T) {
	requirements := section(readREADME(t), "## Requirements")
	if requirements == "" {
		t.Fatal("README has no Requirements section")
	}
	for _, stale := range []string{"libibverbs-dev", "librdmacm-dev", "to build"} {
		if strings.Contains(requirements, stale) {
			t.Errorf("the Requirements section still mentions %q", stale)
		}
	}
	for _, want := range []string{
		"`libibverbs1`", "`librdmacm1t64`", "`ibverbs-providers`", "`librdmacm1`",
		"neither the headers nor a C compiler",
		"native engine",
	} {
		if !strings.Contains(requirements, want) {
			t.Errorf("the Requirements section lacks %q", want)
		}
	}
	for _, caveat := range []string{"rdma-core 53 or later", "`/etc/rdma/rsocket`"} {
		said := sentencesWith(requirements, caveat)
		if len(said) == 0 {
			t.Errorf("the Requirements section lacks %q", caveat)
		}
		for _, s := range said {
			if !strings.Contains(s, "librdmacm engine") {
				t.Errorf("the Requirements section does not confine %q to the librdmacm engine: %q", caveat, s)
			}
		}
	}
}

// TestREADMEPlatforms checks that Platforms names the six supported Linux
// architectures and how each is verified, the independence from cgo, the
// error a host without the libraries gets, the libc the no-cgo build
// targets, and the engine iWARP devices take.
func TestREADMEPlatforms(t *testing.T) {
	platforms := section(readREADME(t), "## Platforms")
	if platforms == "" {
		t.Fatal("README has no Platforms section")
	}
	if strings.Contains(platforms, "Linux with cgo") {
		t.Error("the Platforms section still makes cgo the real implementation")
	}
	for _, want := range []string{
		"amd64", "arm64", "loong64", "ppc64le", "riscv64", "s390x",
		"Soft-RoCE", "without a device", "cross vet", "layout",
		"`CGO_ENABLED`", "errors.ErrUnsupported", "glibc", "musl",
		"linux/386", "linux/arm",
	} {
		if !strings.Contains(platforms, want) {
			t.Errorf("the Platforms section lacks %q", want)
		}
	}
	iwarp := sentencesWith(platforms, "iWARP")
	if len(iwarp) == 0 {
		t.Error("the Platforms section does not mention iWARP")
	}
	for _, s := range iwarp {
		if !strings.Contains(s, "librdmacm engine") {
			t.Errorf("the Platforms section mentions iWARP without the librdmacm engine: %q", s)
		}
	}
}

// TestREADMEBenchmarks checks that the Benchmarks section names the device
// benchmarks of the root package, exactly those, runs the comparison script
// with flags its usage line lists, as the Go workflow does, and names the
// tables and files the script writes and where the workflow shows them.
func TestREADMEBenchmarks(t *testing.T) {
	bench := section(readREADME(t), "## Benchmarks")
	if bench == "" {
		t.Fatal("README has no Benchmarks section")
	}
	declared := benchmarks(t, "BenchmarkRXE")
	if len(declared) == 0 {
		t.Fatal("the root package declares no BenchmarkRXE function")
	}
	named := regexp.MustCompile(`BenchmarkRXE\w+`).FindAllString(bench, -1)
	for _, name := range declared {
		if !slices.Contains(named, name) {
			t.Errorf("the Benchmarks section does not name %s", name)
		}
	}
	for _, name := range named {
		if !slices.Contains(declared, name) {
			t.Errorf("the Benchmarks section names %s, which the root package does not declare", name)
		}
	}
	workflow := readRepoFile(t, ".github", "workflows", "go.yml")
	script := readRepoFile(t, "hack", "benchcompare.sh")
	usage := regexp.MustCompile(`usage: hack/benchcompare\.sh .*`).FindString(script)
	if usage == "" {
		t.Fatal("hack/benchcompare.sh has no usage line")
	}
	flags := regexp.MustCompile(`\[(-\w+)`).FindAllStringSubmatch(usage, -1)
	for what, lines := range map[string][]string{
		"the Benchmarks section": strings.Split(strings.Join(codeBlocks(bench, "sh"), ""), "\n"),
		"the Go workflow":        strings.Split(workflow, "\n"),
	} {
		runs := 0
		for _, line := range lines {
			_, args, ok := strings.Cut(line, "hack/benchcompare.sh")
			if !ok {
				continue
			}
			runs++
			for _, flag := range regexp.MustCompile(`(?:^|\s)(-\w+)`).FindAllStringSubmatch(args, -1) {
				if !slices.ContainsFunc(flags, func(f []string) bool { return f[1] == flag[1] }) {
					t.Errorf("%s runs hack/benchcompare.sh with %s, which is not in its %q", what, flag[1], usage)
				}
			}
		}
		if runs == 0 {
			t.Errorf("%s does not run hack/benchcompare.sh", what)
		}
	}
	for _, quote := range []string{"Compare the benchmarks", "benchmark-comparison", "RDMA_TEST_DEVICE: rxe0"} {
		if !strings.Contains(workflow, quote) {
			t.Errorf("the Go workflow lacks %q, which the Benchmarks section quotes", quote)
		}
	}
	for _, quote := range []string{`"Compare the benchmarks"`, "`benchmark-comparison`", `"Benchmarks on rxe0"`} {
		if !strings.Contains(bench, quote) {
			t.Errorf("the Benchmarks section lacks %s", quote)
		}
	}
	if !strings.Contains(script, "## Benchmarks on $RDMA_TEST_DEVICE") {
		t.Error(`hack/benchcompare.sh does not head the summary with "Benchmarks on" the device`)
	}
	for _, table := range []string{"Implementations", "Noise", "Change"} {
		if !strings.Contains(script, "table "+table+" ") {
			t.Errorf("hack/benchcompare.sh makes no table %q", table)
		}
		if !strings.Contains(bench, `"`+table+`"`) {
			t.Errorf("the Benchmarks section does not name the table %q", table)
		}
	}
	for _, file := range []string{"summary.md", "engines.txt", "base-1.txt", "test.txt", "base-2.txt", "profile.txt", "cpu.pprof"} {
		if !strings.Contains(script, `$out/`+file+`"`) {
			t.Errorf("hack/benchcompare.sh does not write %s", file)
		}
		if !strings.Contains(bench, "`"+file+"`") {
			t.Errorf("the Benchmarks section does not name %s", file)
		}
	}
	for _, want := range []string{"`RDMA_TEST_DEVICE`", "Summary", "baseline", "relative", "rstream"} {
		if !strings.Contains(bench, want) {
			t.Errorf("the Benchmarks section lacks %q", want)
		}
	}
}

// readRepoFile returns the content of a file of the repository.
func readRepoFile(t *testing.T, elem ...string) string {
	t.Helper()
	b, err := os.ReadFile(filepath.Join(append([]string{repoRoot}, elem...)...))
	if err != nil {
		t.Fatal(err)
	}
	return string(b)
}

// TestREADMEBenchmarksOnTwoHosts checks that the Benchmarks section gives
// the commands of a comparison between two hosts: the server and the client
// of pingpong and bw, which take the flags it gives them, and of rstream,
// blocking.
func TestREADMEBenchmarksOnTwoHosts(t *testing.T) {
	lines := strings.Split(strings.Join(codeBlocks(section(readREADME(t), "## Benchmarks"), "sh"), ""), "\n")
	for _, want := range []string{"rdma pingpong -listen ", "rdma pingpong -connect ", "rdma bw -listen ", "rdma bw -connect ", "rstream -b ", "rstream -s "} {
		if !slices.ContainsFunc(lines, func(line string) bool { return strings.HasPrefix(line, want) }) {
			t.Errorf("the Benchmarks section lacks a command %q", want+"…")
		}
	}
	for _, line := range lines {
		args := strings.Fields(line)
		var err error
		switch {
		case len(args) > 2 && args[0] == "rdma" && args[1] == "pingpong":
			_, _, err = parsePingpong(args[2:])
		case len(args) > 2 && args[0] == "rdma" && args[1] == "bw":
			_, _, err = parseBW(args[2:])
		case len(args) > 0 && args[0] == "rstream" && !strings.Contains(line, " -T b"):
			err = errors.New("rstream does not block without -T b")
		}
		if err != nil {
			t.Errorf("the Benchmarks section runs %q: %v", line, err)
		}
	}
}

// TestREADMENamesTheDependencies checks that the README names purego beside
// the standard library and rdma-core, and installs no development package
// anywhere, the Soft-RoCE recipe included.
func TestREADMENamesTheDependencies(t *testing.T) {
	readme := readREADME(t)
	if !strings.Contains(readme, "github.com/ebitengine/purego") {
		t.Error("README does not name github.com/ebitengine/purego")
	}
	for _, stale := range []string{"nothing but the standard library", "libibverbs-dev", "librdmacm-dev"} {
		if strings.Contains(readme, stale) {
			t.Errorf("README still mentions %q", stale)
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

// sentencesWith returns the sentences of doc that contain s.
func sentencesWith(doc, s string) []string {
	var found []string
	for _, line := range strings.Split(doc, "\n") {
		for _, sentence := range strings.SplitAfter(line, ". ") {
			if strings.Contains(sentence, s) {
				found = append(found, sentence)
			}
		}
	}
	return found
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

// benchmarks returns the names of the functions of the root package's test
// files, whatever their build constraints, that begin with prefix.
func benchmarks(t *testing.T, prefix string) []string {
	t.Helper()
	paths, err := filepath.Glob(filepath.Join(repoRoot, "*_test.go"))
	if err != nil {
		t.Fatal(err)
	}
	var names []string
	for _, path := range paths {
		f, err := parser.ParseFile(token.NewFileSet(), path, nil, parser.SkipObjectResolution)
		if err != nil {
			t.Fatal(err)
		}
		for _, d := range f.Decls {
			if fn, ok := d.(*ast.FuncDecl); ok && fn.Recv == nil && strings.HasPrefix(fn.Name.Name, prefix) {
				names = append(names, fn.Name.Name)
			}
		}
	}
	return names
}
