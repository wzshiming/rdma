//go:build linux && cgo

package rdma

import (
	"runtime"
	"syscall"
	"testing"
	"time"

	"github.com/wzshiming/rdma/rsocket"
)

// pipeSocket is a socket whose descriptor is the read end of a pipe, so the
// poller's rpoll sees a plain descriptor; the rest of the interface is never
// called.
type pipeSocket struct {
	socket
	r, w int
}

func newPipeSocket(t *testing.T) *pipeSocket {
	t.Helper()
	var p [2]int
	if err := syscall.Pipe2(p[:], syscall.O_NONBLOCK|syscall.O_CLOEXEC); err != nil {
		t.Fatalf("Pipe2() error: %v", err)
	}
	t.Cleanup(func() {
		syscall.Close(p[0])
		syscall.Close(p[1])
	})
	return &pipeSocket{r: p[0], w: p[1]}
}

func (s *pipeSocket) Descriptor() int { return s.r }

// fill makes the socket readable.
func (s *pipeSocket) fill(t *testing.T) {
	t.Helper()
	if _, err := syscall.Write(s.w, []byte{1}); err != nil {
		t.Fatalf("Write() to the pipe error: %v", err)
	}
}

// expectEvents waits for a delivery on ready and checks it carries want.
func expectEvents(t *testing.T, ready <-chan rsocket.Events, want rsocket.Events) {
	t.Helper()
	select {
	case got := <-ready:
		if got&want == 0 {
			t.Fatalf("delivered %v, want %v", got, want)
		}
	case <-time.After(5 * time.Second):
		t.Fatal("no readiness delivered")
	}
}

// expectSilence checks that nothing is delivered on ready for a while.
func expectSilence(t *testing.T, ready <-chan rsocket.Events) {
	t.Helper()
	select {
	case got := <-ready:
		t.Fatalf("delivered %v, want nothing", got)
	case <-time.After(100 * time.Millisecond):
	}
}

// waitForGoroutines waits until the goroutine count is back to n.
func waitForGoroutines(t *testing.T, n int) {
	t.Helper()
	deadline := time.Now().Add(5 * time.Second)
	for runtime.NumGoroutine() > n && time.Now().Before(deadline) {
		time.Sleep(5 * time.Millisecond)
	}
	if got := runtime.NumGoroutine(); got > n {
		t.Errorf("goroutines = %d, want %d: the poller did not exit", got, n)
	}
}

func TestPollerDeliversReadinessOnce(t *testing.T) {
	p := &poller{}
	s := newPipeSocket(t)
	if err := p.attach(s); err != nil {
		t.Fatalf("attach() error: %v", err)
	}
	ready, _ := p.wait(s, rsocket.PollIn)
	expectSilence(t, ready)
	s.fill(t)
	expectEvents(t, ready, rsocket.PollIn)
	// The interest is spent: a new wait is needed for the next delivery, and
	// the descriptor is still readable, so it comes at once.
	expectSilence(t, ready)
	again, _ := p.wait(s, rsocket.PollIn)
	expectEvents(t, again, rsocket.PollIn)
	p.detach(s)
}

func TestPollerServesTwoWaitersOnOneSocket(t *testing.T) {
	p := &poller{}
	s := newPipeSocket(t)
	if err := p.attach(s); err != nil {
		t.Fatalf("attach() error: %v", err)
	}
	in1, _ := p.wait(s, rsocket.PollIn)
	in2, _ := p.wait(s, rsocket.PollIn)
	s.fill(t)
	expectEvents(t, in1, rsocket.PollIn)
	expectEvents(t, in2, rsocket.PollIn)
	p.detach(s)
}

func TestPollerCancelWithdrawsTheInterest(t *testing.T) {
	p := &poller{}
	s := newPipeSocket(t)
	if err := p.attach(s); err != nil {
		t.Fatalf("attach() error: %v", err)
	}
	ready, cancel := p.wait(s, rsocket.PollIn)
	cancel()
	s.fill(t)
	expectSilence(t, ready)
	p.detach(s)
}

func TestPollerDetachReturnsOnceTheDescriptorIsOut(t *testing.T) {
	p := &poller{}
	s := newPipeSocket(t)
	if err := p.attach(s); err != nil {
		t.Fatalf("attach() error: %v", err)
	}
	ready, _ := p.wait(s, rsocket.PollIn)
	done := make(chan struct{})
	go func() {
		p.detach(s)
		close(done)
	}()
	select {
	case <-done:
	case <-time.After(5 * time.Second):
		t.Fatal("detach() did not return while the poller waited on the socket")
	}
	// The poller no longer knows the socket: readiness is not delivered to
	// the old waiter and a new wait is answered at once.
	s.fill(t)
	expectSilence(t, ready)
	late, _ := p.wait(s, rsocket.PollIn)
	expectEvents(t, late, rsocket.PollNval)
}

// settledGoroutines returns the goroutine count once it has stopped changing,
// so that a poller of an earlier test still on its way out is not counted.
func settledGoroutines() int {
	n := runtime.NumGoroutine()
	for same, i := 0, 0; same < 3 && i < 200; i++ {
		time.Sleep(10 * time.Millisecond)
		if m := runtime.NumGoroutine(); m == n {
			same++
		} else {
			n, same = m, 0
		}
	}
	return n
}

func TestPollerExitsWithTheLastSocket(t *testing.T) {
	p := &poller{}
	before := settledGoroutines()
	a, b := newPipeSocket(t), newPipeSocket(t)
	for _, s := range []*pipeSocket{a, b} {
		if err := p.attach(s); err != nil {
			t.Fatalf("attach() error: %v", err)
		}
	}
	if runtime.NumGoroutine() != before+1 {
		t.Fatalf("goroutines = %d after attaching two sockets, want %d: one poller", runtime.NumGoroutine(), before+1)
	}
	p.detach(a)
	if runtime.NumGoroutine() != before+1 {
		t.Fatalf("goroutines = %d with one socket attached, want %d: the poller stays", runtime.NumGoroutine(), before+1)
	}
	p.detach(b)
	waitForGoroutines(t, before)
	// It comes back for the next socket.
	if err := p.attach(a); err != nil {
		t.Fatalf("attach() after the poller exited error: %v", err)
	}
	ready, _ := p.wait(a, rsocket.PollIn)
	a.fill(t)
	expectEvents(t, ready, rsocket.PollIn)
	p.detach(a)
	waitForGoroutines(t, before)
}
