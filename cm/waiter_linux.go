//go:build linux && cgo

package cm

import (
	"context"
	"errors"
	"os"
	"syscall"
	"time"

	"github.com/wzshiming/rdma/verbs"
)

// aLongTimeAgo is a read deadline that is already over.
var aLongTimeAgo = time.Unix(1, 0)

// fdWaiter waits for an rdma_cm event channel descriptor on the Go runtime
// poller. It wraps a non-blocking dup of the descriptor in an *os.File, whose
// read deadline implements cancellation, so no OS thread blocks inside C.
// Since the two descriptors share their file status flags, O_NONBLOCK also
// makes rdma_get_cm_event fail with EAGAIN instead of blocking.
type fdWaiter struct {
	file *os.File
	raw  syscall.RawConn
	sem  chan struct{} // held by the one goroutine using file; taken cancellably
}

func newFDWaiter(fd int) (*fdWaiter, error) {
	dup, _, errno := syscall.Syscall(syscall.SYS_FCNTL, uintptr(fd), syscall.F_DUPFD_CLOEXEC, 0)
	if errno != 0 {
		return nil, &verbs.OpError{Op: "fcntl", Err: errno}
	}
	if err := syscall.SetNonblock(int(dup), true); err != nil {
		syscall.Close(int(dup))
		return nil, &verbs.OpError{Op: "fcntl", Err: err}
	}
	file := os.NewFile(dup, "rdma-cm-event")
	raw, err := file.SyscallConn()
	if err != nil {
		file.Close()
		return nil, &verbs.OpError{Op: "wait", Err: err}
	}
	return &fdWaiter{file: file, raw: raw, sem: make(chan struct{}, 1)}, nil
}

// wait runs try, which consumes events until it is done or reports
// (false, nil) on EAGAIN, whenever the descriptor is readable, until it
// succeeds, ctx ends or the waiter closes.
func (w *fdWaiter) wait(ctx context.Context, op string, try func() (done bool, err error)) error {
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
		return &verbs.OpError{Op: op, Err: os.ErrClosed}
	}
}

// close wakes the current waiter with an error and makes later waits fail. It
// returns once no try callback is running, so the caller may destroy what try
// uses afterwards.
func (w *fdWaiter) close() { w.file.Close() }
