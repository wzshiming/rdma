//go:build linux && !android && (amd64 || arm64 || loong64 || ppc64le || riscv64 || s390x)

package dynlib

import (
	"runtime"
	"sync"
	"syscall"
	"testing"
	"unsafe"
)

// cint passes n as a C int: sign-extended to the full register.
func cint(n int32) uintptr { return uintptr(int64(n)) }

// TestErrnoSurvivesReschedulingBetweenCallAndRead has goroutines fail libc
// calls with errnos of their own while the seam between a call and the read
// of its errno gives the processor away every time: each goroutine must
// still read the errno of its own call, not one another goroutine's call
// left on the thread it resumes on.
func TestErrnoSurvivesReschedulingBetweenCallAndRead(t *testing.T) {
	lib, err := Open("libc.so.6")
	if err != nil {
		t.Fatal(err)
	}
	lookup := func(name string) Func {
		f, err := lib.Lookup(name)
		if err != nil {
			t.Fatal(err)
		}
		return f
	}
	var pipe [2]int
	if err := syscall.Pipe(pipe[:]); err != nil {
		t.Fatal(err)
	}
	defer syscall.Close(pipe[0])
	defer syscall.Close(pipe[1])
	missing := []byte("/nonexistent/dynlib\x00")
	devnull := []byte("/dev/null\x00")
	minusOne := cint(-1)
	var cwd [1]byte
	close, access, chdir, getcwd, lseek, shutdown := lookup("close"), lookup("access"), lookup("chdir"), lookup("getcwd"), lookup("lseek"), lookup("shutdown")
	calls := []struct {
		name string
		call func() (failed bool, errno syscall.Errno)
		want syscall.Errno
	}{
		{"close(-1)", func() (bool, syscall.Errno) { r, e := close.Int(minusOne, 0, 0, 0, 0, 0); return r == -1, e }, syscall.EBADF},
		{"access(missing)", func() (bool, syscall.Errno) {
			r, e := access.Int(uintptr(unsafe.Pointer(&missing[0])), 0, 0, 0, 0, 0)
			return r == -1, e
		}, syscall.ENOENT},
		{"chdir(/dev/null)", func() (bool, syscall.Errno) {
			r, e := chdir.Int(uintptr(unsafe.Pointer(&devnull[0])), 0, 0, 0, 0, 0)
			return r == -1, e
		}, syscall.ENOTDIR},
		{"getcwd(1 byte)", func() (bool, syscall.Errno) {
			p, e := getcwd.Ptr(uintptr(unsafe.Pointer(&cwd[0])), 1, 0, 0, 0, 0)
			return p == 0, e
		}, syscall.ERANGE},
		{"lseek(pipe)", func() (bool, syscall.Errno) { r, e := lseek.SSize(uintptr(pipe[0]), 0, 0, 0, 0, 0); return r == -1, e }, syscall.ESPIPE},
		{"shutdown(pipe)", func() (bool, syscall.Errno) { r, e := shutdown.Int(uintptr(pipe[0]), 0, 0, 0, 0, 0); return r == -1, e }, syscall.ENOTSOCK},
	}

	testHookBeforeErrno = runtime.Gosched
	defer func() { testHookBeforeErrno = nil }()
	const goroutinesPerCall, rounds = 3, 300
	type outcome struct {
		notFailed, wrongErrno int
		other                 syscall.Errno
	}
	outcomes := make([]outcome, len(calls)*goroutinesPerCall)
	var wg sync.WaitGroup
	for i := range outcomes {
		wg.Add(1)
		go func() {
			defer wg.Done()
			c := calls[i%len(calls)]
			for range rounds {
				failed, errno := c.call()
				switch {
				case !failed:
					outcomes[i].notFailed++
				case errno != c.want:
					outcomes[i].wrongErrno++
					outcomes[i].other = errno
				}
			}
		}()
	}
	wg.Wait()
	for i, o := range outcomes {
		c := calls[i%len(calls)]
		if o.notFailed != 0 {
			t.Errorf("%s did not fail %d times of %d", c.name, o.notFailed, rounds)
		}
		if o.wrongErrno != 0 {
			t.Errorf("%s read another call's errno %d times of %d, e.g. %v instead of %v", c.name, o.wrongErrno, rounds, o.other, c.want)
		}
	}
}
