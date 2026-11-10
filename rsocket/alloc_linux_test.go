//go:build linux && cgo

package rsocket_test

import (
	"syscall"
	"testing"
	"time"

	"github.com/wzshiming/rdma/rsocket"
)

// TestReadWriteAllocateNothing measures the Go heap allocations of Write and
// Read on a connected socket, the hot path of every byte: none, as measured
// with the cgo binding.
func TestReadWriteAllocateNothing(t *testing.T) {
	client, server := pair(t)
	one := make([]byte, 1)
	writes := testing.AllocsPerRun(100, func() {
		if _, err := client.Write(one); err != nil {
			t.Fatalf("Write() error: %v", err)
		}
	})
	t.Logf("Write of 1 byte: %v allocations per call", writes)
	if writes > 0 {
		t.Errorf("Write() allocates %v times per call, want 0", writes)
	}
	// The writes above are read one byte at a time; the data of one Write
	// arrives whole, so none of the reads waits.
	await(t, []rsocket.PollFD{{Fd: server.Descriptor(), Events: rsocket.PollIn}}, time.Now().Add(timeout))
	reads := testing.AllocsPerRun(100, func() {
		if n, err := server.Read(one); n != 1 || err != nil {
			t.Fatalf("Read() = %d, %v; want 1, nil", n, err)
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
