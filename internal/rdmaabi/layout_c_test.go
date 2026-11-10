package rdmaabi

import (
	"errors"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"runtime"
	"slices"
	"strings"
	"testing"
)

// compilers names the C compiler of each supported architecture, found on
// PATH.
var compilers = map[string]string{
	"amd64":   "x86_64-linux-gnu-gcc",
	"arm64":   "aarch64-linux-gnu-gcc",
	"loong64": "loongarch64-linux-gnu-gcc",
	"ppc64le": "powerpc64le-linux-gnu-gcc",
	"riscv64": "riscv64-linux-gnu-gcc",
	"s390x":   "s390x-linux-gnu-gcc",
}

// includeDirs are searched in order for the rdma-core headers
// infiniband/verbs.h and rdma/rdma_cma.h.
var includeDirs = []string{"/usr/local/include", "/usr/include"}

// errToolchain reports a missing compiler or missing headers, which skips
// the check unless RDMA_TEST_LAYOUT_ARCHES demands the architecture.
var errToolchain = errors.New("rdmaabi: C toolchain missing")

// TestLayoutsMatchC has the C compiler of each architecture confirm against
// the installed rdma-core headers that every mirrored field has the offset
// and size of its C member and that every mirror has the size of its C type,
// or at most that size for the leading-fields mirrors. The architectures
// RDMA_TEST_LAYOUT_ARCHES lists must all be checked; unset, the host's own
// is checked when its toolchain is found and skipped otherwise.
func TestLayoutsMatchC(t *testing.T) {
	arches, required := layoutArches(os.Getenv("RDMA_TEST_LAYOUT_ARCHES"))
	for _, arch := range arches {
		t.Run(arch, func(t *testing.T) {
			used, err := checkAgainstC(t.TempDir(), arch)
			if errors.Is(err, errToolchain) && !required {
				t.Skip(err)
			}
			if err != nil {
				t.Fatal(err)
			}
			t.Log(used)
		})
	}
}

func TestLayoutArchesComeFromTheEnvironment(t *testing.T) {
	arches, required := layoutArches("")
	if len(arches) != 1 || arches[0] != runtime.GOARCH || required {
		t.Errorf("unset: got %v required=%v, want [%s] required=false", arches, required, runtime.GOARCH)
	}
	arches, required = layoutArches("amd64, s390x")
	if !required || strings.Join(arches, ",") != "amd64,s390x" {
		t.Errorf("\"amd64, s390x\": got %v required=%v, want [amd64 s390x] required=true", arches, required)
	}
}

func TestMissingCompilerIsAToolchainError(t *testing.T) {
	t.Setenv("PATH", t.TempDir())
	_, err := checkAgainstC(t.TempDir(), "amd64")
	if !errors.Is(err, errToolchain) {
		t.Fatalf("got %v, want errToolchain", err)
	}
}

// layoutArches reads RDMA_TEST_LAYOUT_ARCHES: the architectures to check and
// whether each one must be, a missing toolchain failing instead of skipping.
func layoutArches(env string) (arches []string, required bool) {
	if env == "" {
		return []string{runtime.GOARCH}, false
	}
	for a := range strings.SplitSeq(env, ",") {
		if a = strings.TrimSpace(a); a != "" {
			arches = append(arches, a)
		}
	}
	return arches, true
}

// checkAgainstC compiles, in dir with the compiler of arch, static assertions
// comparing the layout of every mirror with the installed rdma-core headers.
// It returns a line naming the compiler and the headers used.
func checkAgainstC(dir, arch string) (string, error) {
	name, ok := compilers[arch]
	if !ok {
		return "", fmt.Errorf("%w: no C compiler known for GOARCH %s", errToolchain, arch)
	}
	cc, err := exec.LookPath(name)
	if err != nil {
		return "", fmt.Errorf("%w: %s (%s) not on PATH", errToolchain, name, arch)
	}
	include, err := findHeaders()
	if err != nil {
		return "", err
	}
	src := filepath.Join(dir, "layout.c")
	if err := os.WriteFile(src, []byte(assertions()), 0o644); err != nil {
		return "", err
	}
	// The cross compilers do not search /usr/local/include; last, so that
	// the target's own libc headers keep precedence. Preprocess first: a
	// failure there is the toolchain (a cross libc missing), not a layout.
	out, err := exec.Command(cc, "-idirafter", include, "-E", src).CombinedOutput()
	if err != nil {
		return "", fmt.Errorf("%w: %s -E failed: %v\n%s", errToolchain, cc, err, out)
	}
	headers := strings.Join(headerPaths(string(out)), " ")
	out, err = exec.Command(cc, "-idirafter", include, "-fsyntax-only", src).CombinedOutput()
	if err != nil {
		return "", fmt.Errorf("rdmaabi: %s: %s rejects the layout against %s:\n%s", arch, cc, headers, out)
	}
	return fmt.Sprintf("%s: %s confirms the layout against %s", arch, cc, headers), nil
}

// findHeaders returns the first include directory holding both rdma-core
// headers.
func findHeaders() (string, error) {
	for _, dir := range includeDirs {
		_, errVerbs := os.Stat(filepath.Join(dir, "infiniband", "verbs.h"))
		_, errCma := os.Stat(filepath.Join(dir, "rdma", "rdma_cma.h"))
		if errVerbs == nil && errCma == nil {
			return dir, nil
		}
	}
	return "", fmt.Errorf("%w: infiniband/verbs.h and rdma/rdma_cma.h not under %s", errToolchain, strings.Join(includeDirs, " or "))
}

// headerPaths picks the rdma-core headers out of the line markers of
// preprocessor output, such as `# 1 "/usr/include/infiniband/verbs.h" 1 3 4`.
func headerPaths(preprocessed string) []string {
	var paths []string
	for line := range strings.Lines(preprocessed) {
		if !strings.HasPrefix(line, "# ") {
			continue
		}
		first, last := strings.IndexByte(line, '"'), strings.LastIndexByte(line, '"')
		if first < 0 || last <= first {
			continue
		}
		p := line[first+1 : last]
		if (strings.HasSuffix(p, "/infiniband/verbs.h") || strings.HasSuffix(p, "/rdma/rdma_cma.h")) && !slices.Contains(paths, p) {
			paths = append(paths, p)
		}
	}
	return paths
}

// assertions renders a C file whose static assertions hold when every
// mirror has the layout of its C type.
func assertions() string {
	var b strings.Builder
	b.WriteString("#include <stddef.h>\n#include <infiniband/verbs.h>\n#include <rdma/rdma_cma.h>\n\n")
	b.WriteString("#define FIELD(type, member, offset, size) \\\n")
	b.WriteString("\t_Static_assert(offsetof(type, member) == offset, #type \".\" #member \": offset\"); \\\n")
	b.WriteString("\t_Static_assert(sizeof(((type *)0)->member) == size, #type \".\" #member \": size\")\n\n")
	for _, m := range mirrors {
		for _, f := range fields(m.t) {
			fmt.Fprintf(&b, "FIELD(%s, %s, %d, %d);\n", m.c, f.name, f.offset, f.size)
		}
		if m.prefix {
			fmt.Fprintf(&b, "_Static_assert(sizeof(%s) >= %d, \"%s: shorter than its mirror\");\n\n", m.c, m.t.Size(), m.c)
		} else {
			fmt.Fprintf(&b, "_Static_assert(sizeof(%s) == %d, \"%s: size\");\n\n", m.c, m.t.Size(), m.c)
		}
	}
	return b.String()
}
