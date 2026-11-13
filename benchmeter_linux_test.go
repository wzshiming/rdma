//go:build linux && !android && (amd64 || arm64 || loong64 || ppc64le || riscv64 || s390x)

package rdma

import (
	"os"
	"strconv"
	"strings"
	"syscall"
	"testing"
	"time"
)

// startMeter starts the timer of b afresh and returns the function that
// stops it and reports with the time of the ops their allocations and, per
// op, counters of the whole process: syscr/op and syscw/op, the read and the
// write class system calls of /proc/self/io, where each event a completion
// channel delivers is a read; cpu-ns/op, the CPU time, user and system;
// nvcsw/op, the voluntary context switches.
func startMeter(b *testing.B) (stop func()) {
	b.Helper()
	b.ReportAllocs()
	start := readProcCounters(b)
	b.ResetTimer()
	return func() {
		b.Helper()
		b.StopTimer()
		end := readProcCounters(b)
		n := float64(b.N)
		b.ReportMetric(float64(end.syscr-start.syscr)/n, "syscr/op")
		b.ReportMetric(float64(end.syscw-start.syscw)/n, "syscw/op")
		b.ReportMetric(float64(end.cpu-start.cpu)/n, "cpu-ns/op")
		b.ReportMetric(float64(end.nvcsw-start.nvcsw)/n, "nvcsw/op")
	}
}

// procCounters are the counters of this process startMeter reports.
type procCounters struct {
	syscr, syscw int64
	cpu          time.Duration
	nvcsw        int64
}

// readProcCounters reads the counters of this process, failing tb if it
// cannot.
func readProcCounters(tb testing.TB) procCounters {
	tb.Helper()
	var c procCounters
	io, err := os.ReadFile("/proc/self/io")
	if err != nil {
		tb.Fatal(err)
	}
	found := 0
	for line := range strings.Lines(string(io)) {
		name, value, _ := strings.Cut(strings.TrimSpace(line), ": ")
		var field *int64
		switch name {
		case "syscr":
			field = &c.syscr
		case "syscw":
			field = &c.syscw
		default:
			continue
		}
		if *field, err = strconv.ParseInt(value, 10, 64); err != nil {
			tb.Fatalf("/proc/self/io: %v", err)
		}
		found++
	}
	if found != 2 {
		tb.Fatalf("/proc/self/io lacks syscr or syscw:\n%s", io)
	}
	var ru syscall.Rusage
	if err := syscall.Getrusage(syscall.RUSAGE_SELF, &ru); err != nil {
		tb.Fatalf("getrusage: %v", err)
	}
	c.cpu = time.Duration(ru.Utime.Nano() + ru.Stime.Nano())
	c.nvcsw = int64(ru.Nvcsw)
	return c
}

// The meter reports per op at least the read and the write of a pipe each op
// makes, CPU time, and about the one voluntary context switch of its sleep.
func TestMeterReportsCountersPerOp(t *testing.T) {
	r, w, err := os.Pipe()
	if err != nil {
		t.Fatal(err)
	}
	defer r.Close()
	defer w.Close()
	res := testing.Benchmark(func(b *testing.B) {
		buf := make([]byte, 1)
		stop := startMeter(b)
		for range b.N {
			if _, err := w.Write(buf); err != nil {
				b.Fatal(err)
			}
			if _, err := r.Read(buf); err != nil {
				b.Fatal(err)
			}
			syscall.Nanosleep(&syscall.Timespec{Nsec: 1000}, nil)
		}
		stop()
	})
	t.Log(res)
	for _, want := range []struct {
		unit string
		min  float64
	}{
		{"syscr/op", 1},
		{"syscw/op", 1},
		{"cpu-ns/op", 1},
		{"nvcsw/op", 0.5},
	} {
		if got, ok := res.Extra[want.unit]; !ok || got < want.min {
			t.Errorf("%s = %v (reported: %v), want at least %v", want.unit, got, ok, want.min)
		}
	}
}
