//go:build linux && !android && (amd64 || arm64 || loong64 || ppc64le || riscv64 || s390x)

package verbs

import (
	"context"
	"errors"
	"os"
	"runtime"
	"syscall"
	"testing"
	"time"
	"weak"
)

// pipeWaiter is an fdWaiter on the read end of a pipe; try consumes one byte.
type pipeWaiter struct {
	fdWaiter
	r, w int
	next func() (bool, error) // try, made once
	buf  [1]byte              // try's: a local one escapes in race builds
}

func newPipeWaiter(t *testing.T) *pipeWaiter {
	t.Helper()
	p := openPipeWaiter(t)
	t.Cleanup(func() {
		p.close()
		syscall.Close(p.r)
		syscall.Close(p.w)
	})
	return p
}

// openPipeWaiter makes a pipeWaiter that the caller closes.
func openPipeWaiter(t *testing.T) *pipeWaiter {
	t.Helper()
	var fds [2]int
	if err := syscall.Pipe(fds[:]); err != nil {
		t.Fatalf("pipe: %v", err)
	}
	p := &pipeWaiter{r: fds[0], w: fds[1]}
	p.next = p.try
	p.init(p.r)
	if p.err != nil {
		t.Fatalf("init: %v", p.err)
	}
	return p
}

func (p *pipeWaiter) try() (bool, error) {
	_, err := syscall.Read(p.r, p.buf[:])
	if err == syscall.EAGAIN {
		return false, nil
	}
	return true, err
}

func (p *pipeWaiter) wait(ctx context.Context) error {
	if err := p.lock(ctx); err != nil {
		return err
	}
	defer p.unlock()
	return p.fdWaiter.wait(ctx, "pipe", p.next)
}

func TestFDWaiterReturnsOnEvent(t *testing.T) {
	p := newPipeWaiter(t)
	go func() {
		time.Sleep(50 * time.Millisecond)
		syscall.Write(p.w, []byte{1})
	}()
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	if err := p.wait(ctx); err != nil {
		t.Fatalf("wait() error: %v", err)
	}
}

func TestFDWaiterReturnsContextError(t *testing.T) {
	p := newPipeWaiter(t)
	ctx, cancel := context.WithTimeout(context.Background(), 100*time.Millisecond)
	defer cancel()
	start := time.Now()
	if err := p.wait(ctx); !errors.Is(err, context.DeadlineExceeded) {
		t.Fatalf("wait() error = %v, want context.DeadlineExceeded", err)
	}
	if elapsed := time.Since(start); elapsed > 2*time.Second {
		t.Errorf("wait() took %v, want about 100ms", elapsed)
	}
	// The expired deadline of the cancelled wait must not leak into the next one.
	go func() {
		time.Sleep(50 * time.Millisecond)
		syscall.Write(p.w, []byte{1})
	}()
	ctx2, cancel2 := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel2()
	if err := p.wait(ctx2); err != nil {
		t.Fatalf("second wait() error: %v", err)
	}
	done, cancelDone := context.WithCancel(context.Background())
	cancelDone()
	if err := p.wait(done); !errors.Is(err, context.Canceled) {
		t.Errorf("wait(cancelled) error = %v, want context.Canceled", err)
	}
}

func TestFDWaiterCloseWakesWaiter(t *testing.T) {
	p := newPipeWaiter(t)
	errc := make(chan error, 1)
	go func() { errc <- p.wait(context.Background()) }()
	time.Sleep(50 * time.Millisecond)
	p.close()
	select {
	case err := <-errc:
		if !errors.Is(err, os.ErrClosed) {
			t.Errorf("wait() error = %v, want os.ErrClosed", err)
		}
	case <-time.After(5 * time.Second):
		t.Fatal("wait() did not return after close()")
	}
	if err := p.wait(context.Background()); !errors.Is(err, os.ErrClosed) {
		t.Errorf("wait() after close() error = %v, want os.ErrClosed", err)
	}
}

func TestFDWaiterSerializesAndCancelsQueuedWaiters(t *testing.T) {
	p := newPipeWaiter(t)
	first := make(chan error, 1)
	go func() { first <- p.wait(context.Background()) }()
	time.Sleep(50 * time.Millisecond)
	// The second waiter queues behind the first and must still honour its context.
	ctx, cancel := context.WithTimeout(context.Background(), 100*time.Millisecond)
	defer cancel()
	if err := p.wait(ctx); !errors.Is(err, context.DeadlineExceeded) {
		t.Errorf("queued wait() error = %v, want context.DeadlineExceeded", err)
	}
	syscall.Write(p.w, []byte{1})
	select {
	case err := <-first:
		if err != nil {
			t.Errorf("first wait() error: %v", err)
		}
	case <-time.After(5 * time.Second):
		t.Fatal("first wait() did not return after the event")
	}
}

// Waits on one context, as the pump of a native connection makes them, do
// not allocate.
func TestFDWaiterWaitsOnOneContextWithoutAllocating(t *testing.T) {
	p := newPipeWaiter(t)
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	event := []byte{1}
	n := testing.AllocsPerRun(100, func() {
		syscall.Write(p.w, event)
		if err := p.wait(ctx); err != nil {
			t.Fatalf("wait() error: %v", err)
		}
	})
	if n != 0 {
		t.Errorf("wait() on one context allocates %v times per call, want 0", n)
	}
}

// The end of the context of a wait does not end a later wait on another.
func TestFDWaiterOutlivesTheContextOfAnEarlierWait(t *testing.T) {
	p := newPipeWaiter(t)
	first, cancelFirst := context.WithCancel(context.Background())
	syscall.Write(p.w, []byte{1})
	if err := p.wait(first); err != nil {
		t.Fatalf("first wait() error: %v", err)
	}
	cancelFirst()
	go func() {
		time.Sleep(50 * time.Millisecond)
		syscall.Write(p.w, []byte{1})
	}()
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	if err := p.wait(ctx); err != nil {
		t.Fatalf("wait() after the end of the first wait's context: %v", err)
	}
}

// The contexts a waiter waited on may live on after it closes; they must not
// keep it.
func TestFDWaiterClosedIsNotKeptByItsContexts(t *testing.T) {
	before, cancelBefore := context.WithCancel(context.Background())
	defer cancelBefore()
	after, cancelAfter := context.WithCancel(context.Background())
	defer cancelAfter()
	p := waitAndClose(t, before, after)
	runtime.GC()
	if p.Value() != nil {
		t.Error("a context it waited on keeps the closed waiter")
	}
}

// waitAndClose waits on a new pipeWaiter with before, closes it, waits on it
// with after and returns a weak pointer to it.
func waitAndClose(t *testing.T, before, after context.Context) weak.Pointer[pipeWaiter] {
	p := openPipeWaiter(t)
	defer syscall.Close(p.r)
	defer syscall.Close(p.w)
	syscall.Write(p.w, []byte{1})
	if err := p.wait(before); err != nil {
		t.Fatalf("wait() error: %v", err)
	}
	p.close()
	if err := p.wait(after); !errors.Is(err, os.ErrClosed) {
		t.Fatalf("wait() after close() error = %v, want os.ErrClosed", err)
	}
	return weak.Make(p)
}

// The owner of a CQ made elsewhere may destroy it any time, which libibverbs
// lets happen only once all its events are acknowledged.
func TestEventOfAForeignCQIsAcknowledgedAtOnce(t *testing.T) {
	q := &CQ{} // as lookupCQ wraps a CQ that CreateCQ did not make
	for i := range 3 {
		if n := q.tookEvent(); n != 1 {
			t.Fatalf("event %d: tookEvent() = %d, want 1", i+1, n)
		}
	}
}

func TestEventsOfACQAreAcknowledgedInBatches(t *testing.T) {
	q := &CQ{owned: true}
	for i := 1; i < ackBatch; i++ {
		if n := q.tookEvent(); n != 0 {
			t.Fatalf("event %d: tookEvent() = %d, want 0 until a batch is complete", i, n)
		}
	}
	if n := q.tookEvent(); n != ackBatch {
		t.Fatalf("event %d: tookEvent() = %d, want the batch of %d", ackBatch, n, ackBatch)
	}
	if n := q.tookEvent(); n != 0 {
		t.Errorf("event %d: tookEvent() = %d, want 0 as the next batch begins", ackBatch+1, n)
	}
}

func TestCloseTakesTheEventsLeftUnacknowledged(t *testing.T) {
	q := &CQ{owned: true}
	q.tookEvent()
	q.tookEvent()
	if n := q.closeEvents(); n != 2 {
		t.Errorf("closeEvents() = %d, want the 2 events taken", n)
	}
}

// Wait may take an event of a CQ after Close acknowledged the ones before and
// before the CQ is gone, which libibverbs then waits for.
func TestEventOfAClosingCQIsAcknowledgedAtOnce(t *testing.T) {
	q := &CQ{owned: true}
	q.closeEvents()
	if n := q.tookEvent(); n != 1 {
		t.Errorf("tookEvent() while closing = %d, want 1", n)
	}
}
