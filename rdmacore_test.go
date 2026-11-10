package rdma

import (
	"path"
	"strconv"
	"strings"
	"testing"
)

// verdict is what a test that keeps more than three connections alive in
// this process does on the rdma-core it runs against.
type verdict string

const (
	verdictRun   verdict = "run"
	verdictSkip  verdict = "skip"
	verdictFatal verdict = "fatal"
)

// manyConnectionsVerdict decides for the rdma-core release in use (0 when
// unknown) under the environment getenv reads: before 53, once a fourth
// rsocket is registered with one of librdmacm's service threads, the thread
// keeps polling the descriptor array it has just freed and reallocated (fixed
// upstream by 24061f015bd9, "rsockets: Fix for cm_svc_run thread"), after
// which rsockets hang in raccept and rclose or crash, so the test skips, or
// fails under RDMA_TEST_REQUIRE=1 so that CI never skips it silently.
// RDMA_TEST_MANY_CONNS=1 runs it regardless.
func manyConnectionsVerdict(release int, getenv func(string) string) verdict {
	switch {
	case getenv("RDMA_TEST_MANY_CONNS") == "1", release == 0, release >= 53:
		return verdictRun
	case getenv("RDMA_TEST_REQUIRE") == "1":
		return verdictFatal
	}
	return verdictSkip
}

// rdmaCoreReleaseIn reads the rdma-core release from the librdmacm mapped in
// maps, the text of /proc/self/maps, whose file is named
// librdmacm.so.1.<minor>.<release>.<patch>; it returns 0 without one.
func rdmaCoreReleaseIn(maps string) int {
	for _, line := range strings.Split(maps, "\n") {
		fields := strings.Fields(line)
		if len(fields) < 6 {
			continue
		}
		rest, ok := strings.CutPrefix(path.Base(fields[5]), "librdmacm.so.")
		if !ok {
			continue
		}
		parts := strings.Split(rest, ".")
		if len(parts) < 3 {
			continue
		}
		if v, err := strconv.Atoi(parts[2]); err == nil {
			return v
		}
	}
	return 0
}

func TestManyConnectionsVerdict(t *testing.T) {
	tests := []struct {
		name    string
		release int
		env     map[string]string
		want    verdict
	}{
		{"required on 50 fails", 50, map[string]string{"RDMA_TEST_REQUIRE": "1"}, verdictFatal},
		{"50 skips", 50, nil, verdictSkip},
		{"56 runs", 56, nil, verdictRun},
		{"53 runs when required", 53, map[string]string{"RDMA_TEST_REQUIRE": "1"}, verdictRun},
		{"forced on 50 runs", 50, map[string]string{"RDMA_TEST_MANY_CONNS": "1"}, verdictRun},
		{"forced on 50 runs when required", 50, map[string]string{"RDMA_TEST_MANY_CONNS": "1", "RDMA_TEST_REQUIRE": "1"}, verdictRun},
		{"unknown release runs", 0, map[string]string{"RDMA_TEST_REQUIRE": "1"}, verdictRun},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			got := manyConnectionsVerdict(tt.release, func(name string) string { return tt.env[name] })
			if got != tt.want {
				t.Errorf("manyConnectionsVerdict(%d, %v) = %q, want %q", tt.release, tt.env, got, tt.want)
			}
		})
	}
}

func TestRdmaCoreReleaseIn(t *testing.T) {
	tests := []struct {
		name string
		maps string
		want int
	}{
		{
			"librdmacm built from source under /usr/local",
			"7f2b9a000000-7f2b9a01c000 r--p 00000000 08:01 7342                       /usr/local/lib/x86_64-linux-gnu/librdmacm.so.1.5.66.0\n" +
				"7f2b9a01c000-7f2b9a030000 r-xp 0001c000 08:01 7342                       /usr/local/lib/x86_64-linux-gnu/librdmacm.so.1.5.66.0\n",
			66,
		},
		{
			"the distribution's librdmacm",
			"7f2b9a000000-7f2b9a01c000 r--p 00000000 08:01 1835                       /usr/lib/x86_64-linux-gnu/librdmacm.so.1.3.50.0\n",
			50,
		},
		{
			"no librdmacm mapped",
			"7f2b9a000000-7f2b9a01c000 r--p 00000000 08:01 1835                       /usr/lib/x86_64-linux-gnu/libc.so.6\n" +
				"7ffd1a000000-7ffd1a021000 rw-p 00000000 00:00 0                          [stack]\n",
			0,
		},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			if got := rdmaCoreReleaseIn(tt.maps); got != tt.want {
				t.Errorf("rdmaCoreReleaseIn(...) = %d, want %d", got, tt.want)
			}
		})
	}
}
