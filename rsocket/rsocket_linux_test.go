//go:build linux && cgo

package rsocket_test

import (
	"errors"
	"os"
	"syscall"
	"testing"
	"time"

	"github.com/wzshiming/rdma/rsocket"
	"github.com/wzshiming/rdma/verbs"
)

// pipe returns the ends of an OS pipe, closed when the test ends.
func pipe(t *testing.T) (r, w int) {
	t.Helper()
	var p [2]int
	if err := syscall.Pipe(p[:]); err != nil {
		t.Fatalf("Pipe() error: %v", err)
	}
	t.Cleanup(func() {
		syscall.Close(p[0])
		syscall.Close(p[1])
	})
	return p[0], p[1]
}

func TestPollReportsReadablePipe(t *testing.T) {
	r, w := pipe(t)
	if _, err := syscall.Write(w, []byte{1}); err != nil {
		t.Fatalf("Write() error: %v", err)
	}
	fds := []rsocket.PollFD{{Fd: r, Events: rsocket.PollIn}}
	n, err := rsocket.Poll(fds, 2*time.Second)
	if err != nil || n != 1 || fds[0].Revents&rsocket.PollIn == 0 {
		t.Fatalf("Poll() = %d, %v with Revents %v; want 1, nil with POLLIN", n, err, fds[0].Revents)
	}
}

func TestPollTimesOutWithoutEvents(t *testing.T) {
	r, _ := pipe(t)
	fds := []rsocket.PollFD{{Fd: r, Events: rsocket.PollIn}}
	start := time.Now()
	n, err := rsocket.Poll(fds, 100*time.Millisecond)
	elapsed := time.Since(start)
	if err != nil || n != 0 || fds[0].Revents != 0 {
		t.Fatalf("Poll() = %d, %v with Revents %v; want 0, nil with no events", n, err, fds[0].Revents)
	}
	if elapsed < 100*time.Millisecond || elapsed > 2*time.Second {
		t.Errorf("Poll() returned after %v, want about 100ms", elapsed)
	}
}

func TestPollWithoutTimeoutWaitsForWrite(t *testing.T) {
	r, w := pipe(t)
	go func() {
		time.Sleep(50 * time.Millisecond)
		syscall.Write(w, []byte{1})
	}()
	fds := []rsocket.PollFD{{Fd: r, Events: rsocket.PollIn}}
	n, err := rsocket.Poll(fds, -1)
	if err != nil || n != 1 || fds[0].Revents&rsocket.PollIn == 0 {
		t.Fatalf("Poll() = %d, %v with Revents %v; want 1, nil with POLLIN", n, err, fds[0].Revents)
	}
}

func TestPollWithoutDescriptorsIsInvalid(t *testing.T) {
	n, err := rsocket.Poll(nil, 0)
	var opErr *verbs.OpError
	if n != 0 || !errors.Is(err, syscall.EINVAL) || !errors.As(err, &opErr) || opErr.Op != "rpoll" {
		t.Fatalf("Poll(nil) = %d, %#v; want 0 and a *verbs.OpError{Op: \"rpoll\", Err: EINVAL}", n, err)
	}
}

// TestNewReportsErrnoOrOpensSocket accepts both outcomes of New: an errno
// from librdmacm on a host without an RDMA stack, or a socket that closes.
func TestNewReportsErrnoOrOpensSocket(t *testing.T) {
	s, err := rsocket.New(rsocket.IPv4)
	if err != nil {
		var opErr *verbs.OpError
		var errno syscall.Errno
		if !errors.As(err, &opErr) || opErr.Op != "rsocket" || !errors.As(err, &errno) || errno == 0 {
			t.Fatalf("New() error = %#v, want a *verbs.OpError{Op: \"rsocket\", Err: errno}", err)
		}
		t.Logf("New() without an RDMA stack: %v", err)
		return
	}
	t.Logf("New() opened descriptor %d", s.Descriptor())
	if err := s.Close(); err != nil {
		t.Fatalf("Close() error: %v", err)
	}
	if err := s.Close(); !errors.Is(err, os.ErrClosed) {
		t.Errorf("second Close() error = %v, want os.ErrClosed", err)
	}
}
