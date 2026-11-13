//go:build linux

package hack

import (
	"bytes"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"slices"
	"strings"
	"sync"
	"testing"
	"time"
)

// scratch holds what the tests share: the fake repositories and the output
// of the runs of the script on them.
var scratch string

func TestMain(m *testing.M) {
	dir, err := os.MkdirTemp("", "benchcompare")
	if err != nil {
		fmt.Fprintln(os.Stderr, err)
		os.Exit(1)
	}
	scratch = dir
	code := m.Run()
	os.RemoveAll(dir)
	os.Exit(code)
}

// run is a run of the script.
type run struct {
	args           []string
	stdout, stderr string
	err            error
}

// compare runs the script with args, in the test's environment with env on
// top.
func compare(env []string, args ...string) *run {
	cmd := exec.Command("./benchcompare.sh", args...)
	cmd.Env = append(os.Environ(), env...)
	var o, e bytes.Buffer
	cmd.Stdout, cmd.Stderr = &o, &e
	err := cmd.Run()
	return &run{args, o.String(), e.String(), err}
}

func (r *run) String() string {
	return fmt.Sprintf("benchcompare.sh %s: %v\nstderr:\n%s", strings.Join(r.args, " "), r.err, r.stderr)
}

// Without RDMA_TEST_DEVICE the script says that it needs the device and
// exits non-zero.
func TestCompareNeedsADevice(t *testing.T) {
	r := compare([]string{"RDMA_TEST_DEVICE="})
	t.Log(r)
	if r.err == nil {
		t.Error("the script exited 0 without RDMA_TEST_DEVICE")
	}
	if !strings.Contains(r.stderr, "RDMA_TEST_DEVICE") {
		t.Errorf("the script does not say that it needs RDMA_TEST_DEVICE: %q", r.stderr)
	}
}

// fakeBench is the benchmark file of the fake repositories. Their benchmarks
// report figures at once, BenchmarkRXERoundTrip/size=64 on each engine, and
// skip without RDMA_TEST_REQUIRE=1, as the benchmarks of the root package
// skip without a device. Each pass records itself in the file FAKE_LOG
// names, as "<tree> pass", and so does each engine it runs, as "<tree>
// <engine>"; the pass that FAKE_FAIL numbers, counting the passes of both
// trees from 1, fails, the one FAKE_HANG numbers never ends.
const fakeBench = `package fake

import (
	"os"
	"strconv"
	"strings"
	"testing"
	"time"
)

func BenchmarkRXERoundTrip(b *testing.B) {
	if os.Getenv("RDMA_TEST_REQUIRE") != "1" {
		b.Skip("no device")
	}
	switch strconv.Itoa(record(b, "pass")) {
	case os.Getenv("FAKE_FAIL"):
		b.Fatal("failing as FAKE_FAIL tells")
	case os.Getenv("FAKE_HANG"):
		time.Sleep(time.Hour)
	}
	b.Run("size=64", func(b *testing.B) {
		for _, e := range []string{"librdmacm", "native", "rstream"} {
			b.Run("engine="+e, func(b *testing.B) {
				if b.N == 1 {
					record(b, e)
				}
				b.ReportMetric(nsPerOp[e], "ns/op")
			})
		}
	})
}

// only is the benchmark that only one of the trees has.
func only(b *testing.B) {
	b.Run("size=1", func(b *testing.B) {
		b.Run("engine=native", func(b *testing.B) { b.ReportMetric(1, "ns/op") })
	})
}

// record appends "<tree> <what>" to FAKE_LOG and returns the passes it holds.
func record(b *testing.B, what string) int {
	name := os.Getenv("FAKE_LOG")
	f, err := os.OpenFile(name, os.O_WRONLY|os.O_APPEND|os.O_CREATE, 0o644)
	if err != nil {
		b.Fatal(err)
	}
	_, err = f.WriteString(tree + " " + what + "\n")
	if cerr := f.Close(); err == nil {
		err = cerr
	}
	if err != nil {
		b.Fatal(err)
	}
	log, err := os.ReadFile(name)
	if err != nil {
		b.Fatal(err)
	}
	return strings.Count(string(log), " pass\n")
}
`

// fakeTreeFile is the file that tells the trees of a fake repository apart:
// their name, the time an op of their native engine reports and the
// benchmark that only the tree has.
func fakeTreeFile(name string, native int, only string) string {
	return fmt.Sprintf(`package fake

import "testing"

const tree = %q

var nsPerOp = map[string]float64{"librdmacm": 800, "native": %d, "rstream": 300}

func BenchmarkRXEOnly%s(b *testing.B) { only(b) }
`, name, native, only)
}

// fakeRepo makes a git repository of the fake module named name under
// scratch with two commits, the tree of the baseline, "base", and then the
// tree under test, "test", and returns its directory and the commit of the
// baseline.
func fakeRepo(name string) (dir, base string, err error) {
	dir = filepath.Join(scratch, name)
	if err := os.MkdirAll(dir, 0o755); err != nil {
		return "", "", err
	}
	if _, err := git(dir, "init", "-q", "-b", "master"); err != nil {
		return "", "", err
	}
	for i, files := range []map[string]string{
		{"go.mod": "module example.com/fake\n\ngo 1.27\n", "bench_test.go": fakeBench, "tree_test.go": fakeTreeFile("base", 1000, "Base")},
		{"tree_test.go": fakeTreeFile("test", 500, "Test")},
	} {
		for name, content := range files {
			if err := os.WriteFile(filepath.Join(dir, name), []byte(content), 0o644); err != nil {
				return "", "", err
			}
		}
		if _, err := git(dir, "add", "."); err != nil {
			return "", "", err
		}
		if _, err := git(dir, "commit", "-q", "-m", fmt.Sprintf("commit %d", i+1)); err != nil {
			return "", "", err
		}
	}
	base, err = git(dir, "rev-parse", "HEAD~1")
	return dir, base, err
}

// git runs git in dir, away from the configuration of the user and the
// system, and returns what it printed, trimmed.
func git(dir string, args ...string) (string, error) {
	cmd := exec.Command("git", append([]string{"-C", dir}, args...)...)
	cmd.Env = append(os.Environ(), "GIT_CONFIG_GLOBAL=/dev/null", "GIT_CONFIG_NOSYSTEM=1",
		"GIT_AUTHOR_NAME=t", "GIT_AUTHOR_EMAIL=t@example.com", "GIT_COMMITTER_NAME=t", "GIT_COMMITTER_EMAIL=t@example.com")
	out, err := cmd.CombinedOutput()
	if err != nil {
		return "", fmt.Errorf("git %s: %v\n%s", strings.Join(args, " "), err, out)
	}
	return strings.TrimSpace(string(out)), nil
}

// fakeRun is a run of the script on a fake repository.
type fakeRun struct {
	*run
	out string // the -out directory
	log string // what the fake benchmarks recorded
}

// runFake runs the script with args, and then -out and the tree, on the
// fake repository dir in the test's environment with env on top, where
// RDMA_TEST_REQUIRE is cleared so that the script has to set it. The output
// goes to the directory name.out under scratch.
func runFake(name, dir string, env []string, args ...string) *fakeRun {
	out := filepath.Join(scratch, name+".out")
	logFile := filepath.Join(scratch, name+".log")
	env = append([]string{"RDMA_TEST_DEVICE=fake0", "RDMA_TEST_REQUIRE=", "FAKE_LOG=" + logFile}, env...)
	r := compare(env, append(args, "-out", out, dir)...)
	log, _ := os.ReadFile(logFile)
	return &fakeRun{r, out, string(log)}
}

// file returns the content of the file name of the -out directory, failing
// t unless the run succeeded and wrote it.
func (r *fakeRun) file(t *testing.T, name string) string {
	t.Helper()
	if r.err != nil {
		t.Fatal(r)
	}
	b, err := os.ReadFile(filepath.Join(r.out, name))
	if err != nil {
		t.Fatal(err)
	}
	return string(b)
}

var sharedRun = sync.OnceValues(func() (*fakeRun, error) {
	dir, base, err := fakeRepo("shared")
	if err != nil {
		return nil, err
	}
	return runFake("shared", dir, nil, "-base", base), nil
})

// shared returns the run of the script that tests share: ten rounds on the
// tree under test of a fake repository and its baseline.
func shared(t *testing.T) *fakeRun {
	t.Helper()
	r, err := sharedRun()
	if err != nil {
		t.Fatal(err)
	}
	return r
}

// tableSection returns the part of a summary under the heading, up to the
// next heading.
func tableSection(summary, heading string) string {
	_, rest, ok := strings.Cut(summary, "\n"+heading+"\n")
	if !ok {
		return ""
	}
	if i := strings.Index(rest, "\n#"); i >= 0 {
		rest = rest[:i]
	}
	return rest
}

// The summary heads the tables with the device, the trees and the machine.
func TestCompareSummaryHeader(t *testing.T) {
	summary := shared(t).file(t, "summary.md")
	if !strings.HasPrefix(summary, "## Benchmarks on fake0\n") {
		t.Errorf("the summary does not begin with the heading of the device fake0:\n%s", summary)
	}
	for _, row := range []string{
		"| Tree under test | ", "| Rounds | 10 |", "| CPU | ", "| Kernel | ", "| Go | go version ",
		"| rdma-core | ", "| Load average before | ", "| Load average after | ",
	} {
		if !strings.Contains(summary, row) {
			t.Errorf("the summary lacks the row %q:\n%s", row, summary)
		}
	}
}

// The script prints the summary it writes.
func TestCompareShowsTheSummary(t *testing.T) {
	r := shared(t)
	if summary := r.file(t, "summary.md"); r.stdout != summary {
		t.Errorf("the script printed\n%s\nwant the summary\n%s", r.stdout, summary)
	}
}

// The implementations table sets librdmacm and native against rstream, on a
// sample of each engine per round.
func TestCompareImplementations(t *testing.T) {
	table := tableSection(shared(t).file(t, "summary.md"), "### Implementations")
	_, columns, _ := strings.Cut(table, "│")
	columns, _, _ = strings.Cut(columns, "\n")
	r, l, n := strings.Index(columns, "rstream"), strings.Index(columns, "librdmacm"), strings.Index(columns, "native")
	if r < 0 || l < r || n < l {
		t.Errorf("the columns are not rstream, librdmacm and native in turn:\n%s", table)
	}
	for _, want := range []string{"RXERoundTrip/size=64", "vs base", "n=10"} {
		if !strings.Contains(table, want) {
			t.Errorf("the implementations table lacks %q:\n%s", want, table)
		}
	}
}

// The output of the passes of the engines stays in engines.txt, a sample of
// each engine per round.
func TestCompareKeepsTheEnginesOutput(t *testing.T) {
	engines := shared(t).file(t, "engines.txt")
	for _, e := range []string{"librdmacm", "native", "rstream"} {
		if n := strings.Count(engines, "\nBenchmarkRXERoundTrip/size=64/engine="+e+"-"); n != 10 {
			t.Errorf("engines.txt has %d samples of %s, want 10", n, e)
		}
	}
}

// Every pass runs with RDMA_TEST_REQUIRE=1, so that a benchmark that would
// skip fails instead.
func TestComparePassesRequireEveryBenchmark(t *testing.T) {
	if engines := shared(t).file(t, "engines.txt"); strings.Contains(engines, "--- SKIP") {
		t.Errorf("a benchmark skipped:\n%s", engines)
	}
}

// A pass of its own, before the rounds, profiles the native engine of the
// tree under test: profile.txt holds its output, cpu.pprof the profile.
func TestCompareProfilesTheNativeEngine(t *testing.T) {
	r := shared(t)
	if first, _, _ := strings.Cut(r.log, "\n"); first != "test pass" {
		t.Errorf("the first pass recorded %q, want the tree under test's", first)
	}
	profile := r.file(t, "profile.txt")
	if !strings.Contains(profile, "\nBenchmarkRXERoundTrip/size=64/engine=native-") || strings.Contains(profile, "engine=rstream") {
		t.Errorf("profile.txt does not hold a pass of the native engine alone:\n%s", profile)
	}
	if pprof := r.file(t, "cpu.pprof"); pprof == "" {
		t.Error("cpu.pprof is empty")
	}
}

// passes returns the passes a fake log recorded, each as its tree and the
// engines it ran, as in "base: native".
func passes(log string) []string {
	var ps []string
	for line := range strings.Lines(log) {
		tree, what, _ := strings.Cut(strings.TrimSpace(line), " ")
		if what == "pass" {
			ps = append(ps, tree+":")
		} else if len(ps) > 0 {
			ps[len(ps)-1] += " " + what
		}
	}
	return ps
}

// After the profile each round runs the native engine of the baseline, of
// the tree under test and of the baseline again, and then every engine of
// the tree under test.
func TestCompareInterleavesTheTrees(t *testing.T) {
	r := shared(t)
	want := []string{"test: native"}
	for range 10 {
		want = append(want, "base: native", "test: native", "base: native", "test: librdmacm native rstream")
	}
	if got := passes(r.log); !slices.Equal(got, want) {
		t.Errorf("the passes ran\n%s\nwant\n%s", strings.Join(got, "\n"), strings.Join(want, "\n"))
	}
}

// The output of the passes of the native engine stays in a file per group:
// base-1.txt and base-2.txt of the baseline before and after the tree under
// test, test.txt of the tree under test, a sample per round.
func TestCompareKeepsTheNativeOutput(t *testing.T) {
	r := shared(t)
	for _, group := range []string{"base-1.txt", "test.txt", "base-2.txt"} {
		out := r.file(t, group)
		if n := strings.Count(out, "\nBenchmarkRXERoundTrip/size=64/engine=native-"); n != 10 {
			t.Errorf("%s has %d samples of the native engine, want 10", group, n)
		}
		if strings.Contains(out, "engine=librdmacm") || strings.Contains(out, "engine=rstream") {
			t.Errorf("%s has samples of other engines than native:\n%s", group, out)
		}
	}
}

// The header names the baseline.
func TestCompareSummaryNamesTheBaseline(t *testing.T) {
	summary := shared(t).file(t, "summary.md")
	if !strings.Contains(summary, "| Baseline | ") || !strings.Contains(summary, " commit 1 |") {
		t.Errorf("the summary does not name the baseline, commit 1:\n%s", summary)
	}
}

// The noise table sets the native engine of the baseline after the tree under
// test against that before it.
func TestCompareNoise(t *testing.T) {
	table := tableSection(shared(t).file(t, "summary.md"), "### Noise")
	for _, want := range []string{"base-1", "base-2", "RXERoundTrip/size=64/engine=native", "n=10"} {
		if !strings.Contains(table, want) {
			t.Errorf("the noise table lacks %q:\n%s", want, table)
		}
	}
}

// The change table sets the native engine of the tree under test against
// that of the baseline, both groups of it.
func TestCompareChange(t *testing.T) {
	table := tableSection(shared(t).file(t, "summary.md"), "### Change")
	for _, want := range []string{"base", "test", "RXERoundTrip/size=64/engine=native", "-50.00%", "n=20+10"} {
		if !strings.Contains(table, want) {
			t.Errorf("the change table lacks %q:\n%s", want, table)
		}
	}
}

// Only the benchmarks that both trees have make the noise and the change
// tables.
func TestCompareTablesTheBenchmarksOfBothTrees(t *testing.T) {
	summary := shared(t).file(t, "summary.md")
	for _, heading := range []string{"### Noise", "### Change"} {
		if table := tableSection(summary, heading); strings.Contains(table, "RXEOnly") {
			t.Errorf("%s has a benchmark that only one tree has:\n%s", heading, table)
		}
	}
}

// A baseline with the tree of the tree under test makes no rounds of its own
// and no noise or change table, the implementations table alone.
func TestCompareSameTreeTablesTheImplementationsAlone(t *testing.T) {
	dir, _, err := fakeRepo("same")
	if err != nil {
		t.Fatal(err)
	}
	r := runFake("same", dir, nil, "-base", "HEAD", "-rounds", "2")
	want := []string{"test: native", "test: librdmacm native rstream", "test: librdmacm native rstream"}
	if got := passes(r.log); !slices.Equal(got, want) {
		t.Errorf("the passes ran\n%s\nwant\n%s", strings.Join(got, "\n"), strings.Join(want, "\n"))
	}
	summary := r.file(t, "summary.md")
	if !strings.Contains(summary, "\n### Implementations\n") || strings.Contains(summary, "\n### Noise\n") || strings.Contains(summary, "\n### Change\n") {
		t.Errorf("the summary does not have the implementations table alone:\n%s", summary)
	}
}

// checkFailed fails t unless the run failed without a summary, saying why
// in the words of want.
func checkFailed(t *testing.T, r *fakeRun, want ...string) {
	t.Helper()
	if r.err == nil {
		t.Error("the script exited 0")
	}
	for _, w := range want {
		if !strings.Contains(r.stderr, w) {
			t.Errorf("the script does not say %q", w)
		}
	}
	if r.stdout != "" {
		t.Errorf("the script printed a summary:\n%s", r.stdout)
	}
	if _, err := os.Stat(filepath.Join(r.out, "summary.md")); err == nil {
		t.Error("the script wrote summary.md")
	}
}

// A pass that fails fails the comparison, which names the pass, shows its
// output and writes no summary.
func TestCompareFailsWithAPass(t *testing.T) {
	dir, base, err := fakeRepo("fail")
	if err != nil {
		t.Fatal(err)
	}
	r := runFake("fail", dir, []string{"FAKE_FAIL=7"}, "-base", base)
	t.Log(r)
	checkFailed(t, r, "round 2 of 10: the native engine on the tree under test failed", "failing as FAKE_FAIL tells")
}

// A pass that outlasts -timeout gets SIGQUIT, on which Go prints where it
// hangs, and fails the comparison, which names it.
func TestCompareStopsAPassThatHangs(t *testing.T) {
	dir, base, err := fakeRepo("hang")
	if err != nil {
		t.Fatal(err)
	}
	done := make(chan *fakeRun, 1)
	go func() { done <- runFake("hang", dir, []string{"FAKE_HANG=3"}, "-base", base, "-timeout", "2") }()
	select {
	case r := <-done:
		t.Log(r)
		checkFailed(t, r, "round 1 of 10: the native engine on the tree under test did not finish within 2s", "goroutine ")
	case <-time.After(time.Minute):
		t.Fatal("the script did not end within a minute of a pass that hangs")
	}
}

// A pass that runs no benchmark fails the comparison too.
func TestCompareFailsWithAPassOfNoBenchmark(t *testing.T) {
	dir := filepath.Join(scratch, "empty")
	if err := os.MkdirAll(dir, 0o755); err != nil {
		t.Fatal(err)
	}
	for name, content := range map[string]string{"go.mod": "module example.com/fake\n\ngo 1.27\n", "x_test.go": "package fake\n"} {
		if err := os.WriteFile(filepath.Join(dir, name), []byte(content), 0o644); err != nil {
			t.Fatal(err)
		}
	}
	r := runFake("empty", dir, nil, "-rounds", "1")
	t.Log(r)
	checkFailed(t, r, "the profile of the native engine on the tree under test ran no benchmark")
}

// On the tree the script belongs to, its default, the benchmarks fail
// without the device that RDMA_TEST_DEVICE names, and so does the
// comparison, with their complaint about the device.
func TestCompareFailsWithoutTheDevice(t *testing.T) {
	out := filepath.Join(scratch, "absent.out")
	r := &fakeRun{compare([]string{"RDMA_TEST_DEVICE=absent0"}, "-rounds", "1", "-out", out), out, ""}
	t.Log(r)
	checkFailed(t, r, "absent0")
}
