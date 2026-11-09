//go:build linux && cgo

package verbs

/*
#include <infiniband/verbs.h>
*/
import "C"

import (
	"context"
	"errors"
	"os"
	"sync"
	"syscall"
	"time"
	"unsafe"
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
	sem  chan struct{} // held by the one goroutine using file; taken cancellably
}

var errWaiterClosed = &OpError{Op: "wait", Err: os.ErrClosed}

func (w *fdWaiter) init(fd int) {
	w.once.Do(func() {
		w.sem = make(chan struct{}, 1)
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

// wait runs try, which consumes one event or reports (false, nil) on EAGAIN,
// whenever the descriptor is readable, until it succeeds, ctx ends or the
// waiter closes.
func (w *fdWaiter) wait(ctx context.Context, op string, try func() (done bool, err error)) error {
	if w.err != nil {
		return w.err
	}
	if err := ctx.Err(); err != nil {
		return err
	}
	select {
	case w.sem <- struct{}{}:
	case <-ctx.Done():
		return ctx.Err()
	}
	defer func() { <-w.sem }()
	stop := context.AfterFunc(ctx, func() { w.file.SetReadDeadline(aLongTimeAgo) })
	defer stop()
	for {
		var done bool
		var tryErr error
		err := w.raw.Read(func(uintptr) bool {
			done, tryErr = try()
			return done || tryErr != nil
		})
		if err == nil {
			return tryErr
		}
		if cerr := ctx.Err(); cerr != nil {
			w.file.SetReadDeadline(time.Time{})
			return cerr
		}
		if errors.Is(err, os.ErrDeadlineExceeded) {
			// An earlier cancelled wait left its deadline behind.
			w.file.SetReadDeadline(time.Time{})
			continue
		}
		return &OpError{Op: op, Err: os.ErrClosed}
	}
}

// close wakes the current waiter with an error and makes later waits fail.
func (w *fdWaiter) close() {
	w.once.Do(func() { w.err = errWaiterClosed })
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
	c.waiter.init(int(c.ch.fd))
	var cq *CQ
	err := c.waiter.wait(ctx, "ibv_get_cq_event", func() (bool, error) {
		var ccq *C.struct_ibv_cq
		var cqCtx unsafe.Pointer
		for {
			rc, errno := C.ibv_get_cq_event(c.ch, &ccq, &cqCtx)
			if rc == 0 {
				break
			}
			if errno == syscall.EINTR {
				continue
			}
			if errno == syscall.EAGAIN {
				return false, nil
			}
			return false, opError("ibv_get_cq_event", errno)
		}
		C.ibv_ack_cq_events(ccq, 1)
		cq = lookupCQ(ccq)
		return true, nil
	})
	if err != nil {
		return nil, err
	}
	return cq, nil
}

// WaitAsyncEvent waits for the next asynchronous event of the device,
// acknowledges it and returns it. It returns ctx.Err() when ctx ends first and
// an error wrapping os.ErrClosed once the context is closed.
func (c *Context) WaitAsyncEvent(ctx context.Context) (*AsyncEvent, error) {
	if c.ctx == nil {
		return nil, &OpError{Op: "ibv_get_async_event", Err: os.ErrClosed}
	}
	c.async.init(int(c.ctx.async_fd))
	var ev AsyncEvent
	err := c.async.wait(ctx, "ibv_get_async_event", func() (bool, error) {
		var cev C.struct_ibv_async_event
		for {
			rc, errno := C.ibv_get_async_event(c.ctx, &cev)
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
		C.ibv_ack_async_event(&cev)
		return true, nil
	})
	if err != nil {
		return nil, err
	}
	return &ev, nil
}

// asyncEventFromC reads the element union according to the event type.
func asyncEventFromC(cev *C.struct_ibv_async_event) AsyncEvent {
	ev := AsyncEvent{Type: EventType(cev.event_type)}
	elem := unsafe.Pointer(&cev.element)
	switch ev.Type {
	case EventCQErr:
		ev.CQ = lookupCQ(*(**C.struct_ibv_cq)(elem))
	case EventQPFatal, EventQPReqErr, EventQPAccessErr, EventCommEst, EventSQDrained,
		EventPathMig, EventPathMigErr, EventQPLastWQEReached:
		ev.QP = lookupQP(*(**C.struct_ibv_qp)(elem))
	case EventPortActive, EventPortErr, EventLIDChange, EventPkeyChange, EventSMChange,
		EventClientReregister, EventGIDChange:
		ev.Port = uint8(*(*C.int)(elem))
	}
	return ev
}
