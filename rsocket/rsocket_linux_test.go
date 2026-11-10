//go:build linux && !android && (amd64 || arm64 || loong64 || ppc64le || riscv64 || s390x)

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

// TestPollTimesOutAfterAnEarlierTimeout runs the sequence of an event loop:
// a wait that times out, a wait ended by an event, and a wait that must time
// out again. In rdma-core up to v60 the first rpoll timeout leaks its poller
// count and the third call spins forever.
func TestPollTimesOutAfterAnEarlierTimeout(t *testing.T) {
	r, w := pipe(t)
	fds := []rsocket.PollFD{{Fd: r, Events: rsocket.PollIn}}
	if n, err := rsocket.Poll(fds, 20*time.Millisecond); n != 0 || err != nil {
		t.Fatalf("first Poll() = %d, %v; want 0, nil", n, err)
	}
	go func() {
		time.Sleep(20 * time.Millisecond)
		syscall.Write(w, []byte{1})
	}()
	if n, err := rsocket.Poll(fds, -1); n != 1 || err != nil {
		t.Fatalf("second Poll() = %d, %v; want 1, nil", n, err)
	}
	if _, err := syscall.Read(r, make([]byte, 1)); err != nil {
		t.Fatalf("Read() from the pipe error: %v", err)
	}
	done := make(chan struct{})
	var n int
	var err error
	go func() {
		n, err = rsocket.Poll(fds, 100*time.Millisecond)
		close(done)
	}()
	select {
	case <-done:
		if n != 0 || err != nil {
			t.Fatalf("third Poll() = %d, %v; want 0, nil", n, err)
		}
	case <-time.After(5 * time.Second):
		t.Fatal("third Poll(100ms) did not return: an earlier timeout broke rpoll")
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
