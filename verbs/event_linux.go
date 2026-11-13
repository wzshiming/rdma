//go:build linux && !android && (amd64 || arm64 || loong64 || ppc64le || riscv64 || s390x)

package verbs

import (
	"context"
	"encoding/binary"
	"os"
	"sync"
	"syscall"
	"time"
	"unsafe"

	"github.com/wzshiming/rdma/internal/rdmaabi"
)

// aLongTimeAgo is a read deadline that is already over.
var aLongTimeAgo = time.Unix(1, 0)

// fdWaiter waits for an rdma-core event descriptor on the Go runtime poller.
// It wraps a non-blocking dup of the descriptor in an *os.File, whose read
// deadline implements cancellation, so no OS thread blocks inside C. Since the
// two descriptors share their file status flags, O_NONBLOCK also makes the
// rdma-core ibv_get_*_event calls fail with EAGAIN instead of blocking.
type fdWaiter struct {
	once sync.Once
	err  error
	file *os.File
	raw  syscall.RawConn
	sem  chan struct{}      // held by the one goroutine using file; taken cancellably
	read func(uintptr) bool // runs try for raw.Read; made once, as waits must not allocate

	// The holder of sem's.
	try    func() (bool, error)
	tryErr error

	// The end of the context of the last wait expires reads until a wait on
	// another context, so that waits on one context register it only once.
	mu     sync.Mutex
	until  <-chan struct{} // Done of that context
	stop   func() bool     // stops its context.AfterFunc
	closed bool
}

var errWaiterClosed = &OpError{Op: "wait", Err: os.ErrClosed}

func (w *fdWaiter) init(fd int) {
	w.once.Do(func() {
		w.sem = make(chan struct{}, 1)
		w.read = func(uintptr) bool {
			var done bool
			done, w.tryErr = w.try()
			return done || w.tryErr != nil
		}
		dup, _, errno := syscall.Syscall(syscall.SYS_FCNTL, uintptr(fd), syscall.F_DUPFD_CLOEXEC, 0)
		if errno != 0 {
			w.err = &OpError{Op: "fcntl", Err: errno}
			return
		}
		if err := syscall.SetNonblock(int(dup), true); err != nil {
			syscall.Close(int(dup))
			w.err = &OpError{Op: "fcntl", Err: err}
			return
		}
		w.file = os.NewFile(dup, "rdma-event")
		raw, err := w.file.SyscallConn()
		if err != nil {
			w.file.Close()
			w.err = &OpError{Op: "wait", Err: err}
			return
		}
		w.raw = raw
	})
}

// lock takes the waiter for the calling goroutine, unless ctx ends first. Until
// unlock, the caller may wait and read what try left.
func (w *fdWaiter) lock(ctx context.Context) error {
	if w.err != nil {
		return w.err
	}
	if err := ctx.Err(); err != nil {
		return err
	}
	select {
	case w.sem <- struct{}{}:
		return nil
	case <-ctx.Done():
		return ctx.Err()
	}
}

func (w *fdWaiter) unlock() { <-w.sem }

// wait runs try, which consumes one event or reports (false, nil) on EAGAIN,
// whenever the descriptor is readable, until it succeeds, ctx ends or the
// waiter closes. The caller holds the waiter, and try is made once: waits on
// one context then do not allocate.
func (w *fdWaiter) wait(ctx context.Context, op string, try func() (done bool, err error)) error {
	w.watch(ctx)
	w.try = try
	if err := w.raw.Read(w.read); err != nil {
		if cerr := ctx.Err(); cerr != nil {
			return cerr
		}
		return &OpError{Op: op, Err: os.ErrClosed}
	}
	return w.tryErr
}

// watch has the end of ctx expire reads from now until a wait on another
// context. Only the context of the last wait can expire them: watch clears
// the deadline an earlier context left, and an AfterFunc that runs late leaves
// it alone. The caller holds the waiter.
func (w *fdWaiter) watch(ctx context.Context) {
	done := ctx.Done()
	if done == w.until {
		return
	}
	w.mu.Lock()
	defer w.mu.Unlock()
	if w.stop != nil {
		w.stop()
		w.stop = nil
	}
	w.until = done
	w.file.SetReadDeadline(time.Time{})
	if done == nil || w.closed {
		return
	}
	w.stop = context.AfterFunc(ctx, func() {
		w.mu.Lock()
		defer w.mu.Unlock()
		if w.until == done {
			w.file.SetReadDeadline(aLongTimeAgo)
		}
	})
}

// close wakes the current waiter with an error and makes later waits fail.
// No context keeps the waiter from then on.
func (w *fdWaiter) close() {
	w.once.Do(func() { w.err = errWaiterClosed })
	w.mu.Lock()
	w.closed = true
	if w.stop != nil {
		w.stop()
		w.stop = nil
	}
	w.mu.Unlock()
	if w.file != nil {
		w.file.Close()
	}
}

// Wait waits for the next completion event on the channel, acknowledges it
// and returns the CQ it belongs to: the *CQ from CreateCQ, or a new non-owning
// wrapper for a CQ created elsewhere. It returns ctx.Err() when ctx ends first
// and an error wrapping os.ErrClosed once the channel is closed.
func (c *CompChannel) Wait(ctx context.Context) (*CQ, error) {
	if c.ch == nil {
		return nil, &OpError{Op: "ibv_get_cq_event", Err: os.ErrClosed}
	}
	f, err := load("ibv_get_cq_event")
	if err != nil {
		return nil, err
	}
	c.waiter.init(int(c.ch.Fd))
	if err := c.waiter.lock(ctx); err != nil {
		return nil, err
	}
	defer c.waiter.unlock()
	if err := c.waiter.wait(ctx, "ibv_get_cq_event", c.next); err != nil {
		return nil, err
	}
	addr := uintptr(c.event.cq)
	q := lookupCQ(addr)
	if n := q.tookEvent(); n > 0 {
		f.ackCQEvents.Int(addr, uintptr(n), 0, 0, 0, 0)
	}
	return q, nil
}

// nextEvent takes the next event off the channel into c.event, or reports
// (false, nil) when there is none yet.
func (c *CompChannel) nextEvent() (bool, error) {
	f, err := load("ibv_get_cq_event")
	if err != nil {
		return false, err
	}
	for {
		rc, errno := f.getCQEvent.Int(caddr(c.ch), uintptr(unsafe.Pointer(&c.event.cq)), uintptr(unsafe.Pointer(&c.event.cqContext)), 0, 0, 0)
		if rc == 0 {
			return true, nil
		}
		if errno == syscall.EINTR {
			continue
		}
		if errno == syscall.EAGAIN {
			return false, nil
		}
		return false, opError("ibv_get_cq_event", errno)
	}
}

// WaitAsyncEvent waits for the next asynchronous event of the device,
// acknowledges it and returns it. It returns ctx.Err() when ctx ends first and
// an error wrapping os.ErrClosed once the context is closed.
func (c *Context) WaitAsyncEvent(ctx context.Context) (*AsyncEvent, error) {
	if c.ctx == nil {
		return nil, &OpError{Op: "ibv_get_async_event", Err: os.ErrClosed}
	}
	f, err := load("ibv_get_async_event")
	if err != nil {
		return nil, err
	}
	c.async.init(int(c.ctx.Async_fd))
	if err := c.async.lock(ctx); err != nil {
		return nil, err
	}
	defer c.async.unlock()
	var ev AsyncEvent
	err = c.async.wait(ctx, "ibv_get_async_event", func() (bool, error) {
		var cev rdmaabi.IbvAsyncEvent
		for {
			rc, errno := f.getAsyncEvent.Int(caddr(c.ctx), uintptr(unsafe.Pointer(&cev)), 0, 0, 0, 0)
			if rc == 0 {
				break
			}
			if errno == syscall.EINTR {
				continue
			}
			if errno == syscall.EAGAIN {
				return false, nil
			}
			return false, opError("ibv_get_async_event", errno)
		}
		ev = asyncEventFromC(&cev)
		f.ackAsyncEvent.Int(uintptr(unsafe.Pointer(&cev)), 0, 0, 0, 0, 0)
		return true, nil
	})
	if err != nil {
		return nil, err
	}
	return &ev, nil
}

// asyncEventFromC reads the element union according to the event type: a
// pointer for CQ and QP events, an int port number for port events.
func asyncEventFromC(cev *rdmaabi.IbvAsyncEvent) AsyncEvent {
	ev := AsyncEvent{Type: EventType(cev.Event_type)}
	switch ev.Type {
	case EventCQErr:
		ev.CQ = lookupCQ(uintptr(binary.NativeEndian.Uint64(cev.Element[:])))
	case EventQPFatal, EventQPReqErr, EventQPAccessErr, EventCommEst, EventSQDrained,
		EventPathMig, EventPathMigErr, EventQPLastWQEReached:
		ev.QP = lookupQP(uintptr(binary.NativeEndian.Uint64(cev.Element[:])))
	case EventPortActive, EventPortErr, EventLIDChange, EventPkeyChange, EventSMChange,
		EventClientReregister, EventGIDChange:
		ev.Port = uint8(binary.NativeEndian.Uint32(cev.Element[:4]))
	}
	return ev
}
