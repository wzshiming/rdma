//go:build linux && !android && !race && (amd64 || arm64 || loong64 || ppc64le || riscv64 || s390x)

package rsocket_test

import (
	"syscall"
	"testing"
	"time"

	"github.com/wzshiming/rdma/internal/rsocket"
)

// Under the race detector sync.Pool, which dynlib draws the argument block
// of every call from, drops entries at random, so these tests stay out of
// race builds.

// TestReadWriteAllocateNothing measures the Go heap allocations of Write and
// Read on a connected socket, the hot path of every byte: none, as measured
// with the cgo binding.
func TestReadWriteAllocateNothing(t *testing.T) {
	client, server := pair(t)
	const runs = 100 // after AllocsPerRun's warm-up run
	one := make([]byte, 1)
	writes := testing.AllocsPerRun(runs, func() {
		if _, err := client.Write(one); err != nil {
			t.Fatalf("Write() error: %v", err)
		}
	})
	t.Logf("Write of 1 byte: %v allocations per call", writes)
	if writes > 0 {
		t.Errorf("Write() allocates %v times per call, want 0", writes)
	}
	// The bytes written above arrive in as many messages, which trickle in,
	// so they are drained first; one Write of as many bytes then arrives as
	// one message, which the reads take a byte at a time without waiting.
	readFull(t, server, make([]byte, runs+1))
	writeAll(t, client, make([]byte, runs+1))
	await(t, []rsocket.PollFD{{Fd: server.Descriptor(), Events: rsocket.PollIn}}, time.Now().Add(timeout))
	reads := testing.AllocsPerRun(runs, func() {
		if got, err := server.Read(one); got != 1 || err != nil {
			t.Fatalf("Read() = %d, %v; want 1, nil", got, err)
		}
	})
	t.Logf("Read of 1 byte: %v allocations per call", reads)
	if reads > 0 {
		t.Errorf("Read() allocates %v times per call, want 0", reads)
	}
}

// TestPollAllocatesOnce measures the Go heap allocations of Poll on a
// readable pipe, without a timeout as the poller of the root package calls
// it and with one, which adds a timerfd: one for the descriptor array, as
// measured with the cgo binding.
func TestPollAllocatesOnce(t *testing.T) {
	r, w := pipe(t)
	if _, err := syscall.Write(w, []byte{1}); err != nil {
		t.Fatalf("Write() to the pipe error: %v", err)
	}
	fds := []rsocket.PollFD{{Fd: r, Events: rsocket.PollIn}}
	for _, tt := range []struct {
		name    string
		timeout time.Duration
	}{
		{"without timeout", -1},
		{"with timeout", time.Second},
	} {
		t.Run(tt.name, func(t *testing.T) {
			allocs := testing.AllocsPerRun(100, func() {
				if n, err := rsocket.Poll(fds, tt.timeout); n != 1 || err != nil {
					t.Fatalf("Poll() = %d, %v; want 1, nil", n, err)
				}
			})
			t.Logf("Poll on a readable pipe %s: %v allocations per call", tt.name, allocs)
			if allocs > 1 {
				t.Errorf("Poll() allocates %v times per call, want at most 1", allocs)
			}
		})
	}
}
