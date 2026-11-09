//go:build linux && cgo

package cm

import (
	"context"
	"errors"
	"os"
	"syscall"
	"testing"
	"time"
)

// pipeWaiter is an fdWaiter on the read end of a pipe; try consumes one byte.
type pipeWaiter struct {
	*fdWaiter
	r, w int
}

func newPipeWaiter(t *testing.T) *pipeWaiter {
	t.Helper()
	var fds [2]int
	if err := syscall.Pipe(fds[:]); err != nil {
		t.Fatalf("pipe: %v", err)
	}
	w, err := newFDWaiter(fds[0])
	if err != nil {
		t.Fatalf("newFDWaiter() error: %v", err)
	}
	p := &pipeWaiter{fdWaiter: w, r: fds[0], w: fds[1]}
	t.Cleanup(func() {
		p.close()
		syscall.Close(p.r)
		syscall.Close(p.w)
	})
	return p
}

func (p *pipeWaiter) try() (bool, error) {
	var b [1]byte
	_, err := syscall.Read(p.r, b[:])
	if err == syscall.EAGAIN {
		return false, nil
	}
	return true, err
}

func (p *pipeWaiter) wait(ctx context.Context) error { return p.fdWaiter.wait(ctx, "pipe", p.try) }

func TestFDWaiterMakesTheDescriptorNonBlocking(t *testing.T) {
	p := newPipeWaiter(t)
	// The dup shares the file status flags, so the original descriptor must
	// now report EAGAIN instead of blocking.
	var b [1]byte
	if _, err := syscall.Read(p.r, b[:]); err != syscall.EAGAIN {
		t.Fatalf("read on the empty pipe: %v, want EAGAIN", err)
	}
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
