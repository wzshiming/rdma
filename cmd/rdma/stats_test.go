package main

import (
	"bytes"
	"math"
	"testing"
	"time"
)

func ms(ds ...int) []time.Duration {
	out := make([]time.Duration, len(ds))
	for i, d := range ds {
		out[i] = time.Duration(d) * time.Millisecond
	}
	return out
}

func TestPercentileNearestRank(t *testing.T) {
	tests := []struct {
		name   string
		sorted []time.Duration
		p      float64
		want   time.Duration
	}{
		{"median of an even count is the upper middle sample", ms(1, 2, 3, 4, 5, 6, 7, 8, 9, 10), 50, 5 * time.Millisecond},
		{"median of an odd count is the middle sample", ms(1, 2, 3), 50, 2 * time.Millisecond},
		{"p99 of ten samples is the last", ms(1, 2, 3, 4, 5, 6, 7, 8, 9, 10), 99, 10 * time.Millisecond},
		{"p99 of two hundred samples is the 198th", ramp(200), 99, 198 * time.Millisecond},
		{"p100 is the maximum", ms(1, 2, 3), 100, 3 * time.Millisecond},
		{"p1 of ten samples is the first", ms(1, 2, 3, 4, 5, 6, 7, 8, 9, 10), 1, time.Millisecond},
		{"a single sample is every percentile", ms(7), 99, 7 * time.Millisecond},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			if got := percentile(tt.sorted, tt.p); got != tt.want {
				t.Errorf("percentile(%v, %v) = %v, want %v", tt.sorted, tt.p, got, tt.want)
			}
		})
	}
}

// ramp returns 1ms, 2ms, ... n ms.
func ramp(n int) []time.Duration {
	ds := make([]int, n)
	for i := range ds {
		ds[i] = i + 1
	}
	return ms(ds...)
}

func TestSummarizeLatency(t *testing.T) {
	got := summarizeLatency(ms(5, 1, 3), time.Second)
	want := latency{Iterations: 3, Min: time.Millisecond, Median: 3 * time.Millisecond, P99: 5 * time.Millisecond, Max: 5 * time.Millisecond, Elapsed: time.Second}
	if got != want {
		t.Errorf("summarizeLatency(5ms, 1ms, 3ms; 1s) = %+v, want %+v", got, want)
	}
	if rate := got.perSecond(); rate != 3 {
		t.Errorf("perSecond() = %v, want 3", rate)
	}
}

func TestSummarizeLatencyLeavesSamplesUnsorted(t *testing.T) {
	samples := ms(5, 1, 3)
	summarizeLatency(samples, time.Second)
	if samples[0] != 5*time.Millisecond || samples[1] != time.Millisecond {
		t.Errorf("samples = %v after summarizeLatency, want them untouched", samples)
	}
}

func TestLatencyWrite(t *testing.T) {
	l := latency{Iterations: 3, Min: time.Millisecond, Median: 3 * time.Millisecond, P99: 5 * time.Millisecond, Max: 5 * time.Millisecond, Elapsed: 2 * time.Second}
	var buf bytes.Buffer
	l.write(&buf)
	want := "iterations: 3\nmin: 1ms\nmedian: 3ms\np99: 5ms\nmax: 5ms\nmsg/s: 1.50\n"
	if buf.String() != want {
		t.Errorf("latency.write() wrote\n%s\nwant\n%s", buf.String(), want)
	}
}

func TestThroughputRates(t *testing.T) {
	tests := []struct {
		name      string
		tp        throughput
		mib, gbit float64
	}{
		{"one GiB per second", throughput{Bytes: 1 << 30, Duration: time.Second}, 1024, 8.589934592},
		{"one GB in two seconds", throughput{Bytes: 1e9, Duration: 2 * time.Second}, 1e9 / (1 << 20) / 2, 4},
		{"half a second", throughput{Bytes: 1 << 20, Duration: 500 * time.Millisecond}, 2, 2 * 8 * (1 << 20) / 1e9},
		{"zero duration is zero rather than infinite", throughput{Bytes: 1 << 20}, 0, 0},
		{"no bytes", throughput{Duration: time.Second}, 0, 0},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			if got := tt.tp.mibPerSecond(); !near(got, tt.mib) {
				t.Errorf("%+v.mibPerSecond() = %v, want %v", tt.tp, got, tt.mib)
			}
			if got := tt.tp.gbitPerSecond(); !near(got, tt.gbit) {
				t.Errorf("%+v.gbitPerSecond() = %v, want %v", tt.tp, got, tt.gbit)
			}
		})
	}
}

func near(a, b float64) bool { return math.Abs(a-b) < 1e-9 }

func TestThroughputWrite(t *testing.T) {
	tests := []struct {
		name string
		tp   throughput
		want string
	}{
		{"one GiB per second", throughput{Bytes: 1 << 30, Duration: time.Second}, "bytes: 1073741824\nduration: 1s\nMiB/s: 1024.00\nGbit/s: 8.590\n"},
		{"fractions keep two and three decimals", throughput{Bytes: 3 << 20, Duration: 2 * time.Second}, "bytes: 3145728\nduration: 2s\nMiB/s: 1.50\nGbit/s: 0.013\n"},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			var buf bytes.Buffer
			tt.tp.write(&buf)
			if buf.String() != tt.want {
				t.Errorf("throughput.write() wrote\n%s\nwant\n%s", buf.String(), tt.want)
			}
		})
	}
}
