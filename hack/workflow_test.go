//go:build linux

package hack

import (
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
)

// chooseBaseline is what the comparison step of the integration job runs to
// choose the baseline, base, and say why, with PREVIOUS_TIP the tip of the
// branch before a push and empty on a pull request.
const chooseBaseline = `base=$(git merge-base HEAD origin/master) why="the merge base with master"
if git cat-file -e "$PREVIOUS_TIP^{commit}" 2>/dev/null; then base=$PREVIOUS_TIP why="the tip of the branch before the push"; fi
`

// The integration job compares a push to an existing branch with the tip of
// the branch before the push, and a new branch, a forced push that left
// that tip behind and a pull request, which checks out the merge into
// master, with the merge base with master.
func TestWorkflowChoosesTheBaseline(t *testing.T) {
	workflow, err := os.ReadFile(filepath.Join("..", ".github", "workflows", "go.yml"))
	if err != nil {
		t.Fatal(err)
	}
	for _, line := range append(strings.Split(strings.TrimSpace(chooseBaseline), "\n"),
		"PREVIOUS_TIP: ${{ github.event_name == 'push' && github.event.before || '' }}",
		`hack/benchcompare.sh -base "$base"`,
	) {
		if !strings.Contains(string(workflow), line) {
			t.Errorf("the Go workflow lacks %q", line)
		}
	}

	// master: m1, m2; the branch: f1 and f2 on m1; the pull request: f2 merged into m2.
	repo := filepath.Join(scratch, "baseline")
	if err := os.MkdirAll(repo, 0o755); err != nil {
		t.Fatal(err)
	}
	run := func(args ...string) string {
		t.Helper()
		out, err := git(repo, args...)
		if err != nil {
			t.Fatal(err)
		}
		return out
	}
	commit := func(args ...string) string {
		t.Helper()
		run(args...)
		return run("rev-parse", "HEAD")
	}
	run("init", "-q", "-b", "master")
	m1 := commit("commit", "-q", "--allow-empty", "-m", "m1")
	m2 := commit("commit", "-q", "--allow-empty", "-m", "m2")
	run("update-ref", "refs/remotes/origin/master", m2)
	run("checkout", "-q", "-b", "branch", m1)
	f1 := commit("commit", "-q", "--allow-empty", "-m", "f1")
	f2 := commit("commit", "-q", "--allow-empty", "-m", "f2")
	run("checkout", "-q", "--detach", m2)
	merge := commit("merge", "-q", "--no-ff", "-m", "merge", f2)

	for _, tt := range []struct {
		event          string
		head           string
		previous, want string
	}{
		{"a push to an existing branch", f2, f1, f1},
		{"a push of a new branch", f2, strings.Repeat("0", 40), m1},
		{"a forced push that left the previous tip behind", f2, strings.Repeat("1", 40), m1},
		{"a pull request", merge, "", m2},
	} {
		t.Run(tt.event, func(t *testing.T) {
			run("checkout", "-q", "--detach", tt.head)
			cmd := exec.Command("bash", "-e", "-c", chooseBaseline+`echo "$base"`)
			cmd.Dir = repo
			cmd.Env = append(os.Environ(), "PREVIOUS_TIP="+tt.previous)
			out, err := cmd.Output()
			if err != nil {
				t.Fatalf("choosing the baseline: %v", err)
			}
			if got := strings.TrimSpace(string(out)); got != tt.want {
				t.Errorf("the baseline is %s, want %s", got, tt.want)
			}
		})
	}
}
