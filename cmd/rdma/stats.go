package main

import (
	"fmt"
	"io"
	"math"
	"slices"
	"time"
)

// percentile returns the p-th percentile, 0 < p <= 100, of the ascending
// samples by the nearest-rank method: the sample at rank ceil(p/100 x n),
// so for an even count the median is the upper of the two middle samples
// and p100 is the maximum.
func percentile(sorted []time.Duration, p float64) time.Duration {
	rank := int(math.Ceil(p * float64(len(sorted)) / 100))
	return sorted[max(rank, 1)-1]
}

// latency summarizes the round trips of a pingpong run.
type latency struct {
	Iterations            int
	Min, Median, P99, Max time.Duration
	Elapsed               time.Duration // wall time of the whole run
}

// summarizeLatency summarizes round-trip samples measured within elapsed.
func summarizeLatency(samples []time.Duration, elapsed time.Duration) latency {
	sorted := slices.Clone(samples)
	slices.Sort(sorted)
	return latency{
		Iterations: len(sorted),
		Min:        sorted[0],
		Median:     percentile(sorted, 50),
		P99:        percentile(sorted, 99),
		Max:        sorted[len(sorted)-1],
		Elapsed:    elapsed,
	}
}

// perSecond returns the round trips per second over the run.
func (l latency) perSecond() float64 { return rate(float64(l.Iterations), l.Elapsed) }

func (l latency) write(w io.Writer) {
	fmt.Fprintf(w, "iterations: %d\n", l.Iterations)
	fmt.Fprintf(w, "min: %v\n", l.Min)
	fmt.Fprintf(w, "median: %v\n", l.Median)
	fmt.Fprintf(w, "p99: %v\n", l.P99)
	fmt.Fprintf(w, "max: %v\n", l.Max)
	fmt.Fprintf(w, "msg/s: %.2f\n", l.perSecond())
}

// throughput is the amount of data a bw run moved and how long it took.
type throughput struct {
	Bytes    int64
	Duration time.Duration
}

func (t throughput) mibPerSecond() float64 { return rate(float64(t.Bytes)/(1<<20), t.Duration) }

// gbitPerSecond returns the rate in decimal gigabits (10^9 bits) per second.
func (t throughput) gbitPerSecond() float64 { return rate(float64(t.Bytes)*8/1e9, t.Duration) }

func (t throughput) write(w io.Writer) {
	fmt.Fprintf(w, "bytes: %d\n", t.Bytes)
	fmt.Fprintf(w, "duration: %v\n", t.Duration)
	fmt.Fprintf(w, "MiB/s: %.2f\n", t.mibPerSecond())
	fmt.Fprintf(w, "Gbit/s: %.3f\n", t.gbitPerSecond())
}

// rate returns amount per second, 0 when no time passed.
func rate(amount float64, d time.Duration) float64 {
	if d <= 0 {
		return 0
	}
	return amount / d.Seconds()
}
