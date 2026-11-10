//go:build linux && !android && (amd64 || arm64 || loong64 || ppc64le || riscv64 || s390x)

package dynlib_test

import (
	"errors"
	"os"
	"strings"
	"sync"
	"syscall"
	"testing"
	"unsafe"

	"github.com/wzshiming/rdma/internal/dynlib"
)

// The tests call libc, which every process on a supported platform carries.
const libc = "libc.so.6"

// cint passes n as a C int: sign-extended to the full register.
func cint(n int32) uintptr { return uintptr(int64(n)) }

// cstring returns s as a NUL-terminated C string.
func cstring(s string) []byte { return append([]byte(s), 0) }

func openLibc(t *testing.T) *dynlib.Lib {
	t.Helper()
	lib, err := dynlib.Open(libc)
	if err != nil {
		t.Fatal(err)
	}
	return lib
}

func libcFunc(t *testing.T, name string) dynlib.Func {
	t.Helper()
	f, err := openLibc(t).Lookup(name)
	if err != nil {
		t.Fatal(err)
	}
	return f
}

func pipe(t *testing.T) (r, w int) {
	t.Helper()
	var p [2]int
	if err := syscall.Pipe(p[:]); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { syscall.Close(p[0]); syscall.Close(p[1]) })
	return p[0], p[1]
}

func TestIntCallReturnsGetpid(t *testing.T) {
	pid, errno := libcFunc(t, "getpid").Int(0, 0, 0, 0, 0, 0)
	if errno != 0 {
		t.Fatalf("getpid() errno = %v, want 0", errno)
	}
	if int(pid) != os.Getpid() {
		t.Fatalf("getpid() = %d, want %d", pid, os.Getpid())
	}
}

func TestFailedCallReturnsItsErrno(t *testing.T) {
	t.Run("close of an invalid descriptor", func(t *testing.T) {
		r, errno := libcFunc(t, "close").Int(cint(-1), 0, 0, 0, 0, 0)
		if r != -1 || errno != syscall.EBADF {
			t.Fatalf("close(-1) = %d, %v; want -1, EBADF", r, errno)
		}
	})
	t.Run("access of a missing path", func(t *testing.T) {
		path := cstring("/nonexistent/dynlib")
		r, errno := libcFunc(t, "access").Int(uintptr(unsafe.Pointer(&path[0])), 0, 0, 0, 0, 0)
		if r != -1 || errno != syscall.ENOENT {
			t.Fatalf("access(%q, F_OK) = %d, %v; want -1, ENOENT", path[:len(path)-1], r, errno)
		}
	})
}

func TestSSizeCall(t *testing.T) {
	_, w := pipe(t)
	write := libcFunc(t, "write")
	msg := []byte("dynlib")
	n, errno := write.SSize(uintptr(w), uintptr(unsafe.Pointer(&msg[0])), uintptr(len(msg)), 0, 0, 0)
	if n != len(msg) || errno != 0 {
		t.Fatalf("write(pipe, %q) = %d, %v; want %d, 0", msg, n, errno, len(msg))
	}
	n, errno = write.SSize(cint(-1), uintptr(unsafe.Pointer(&msg[0])), uintptr(len(msg)), 0, 0, 0)
	if n != -1 || errno != syscall.EBADF {
		t.Fatalf("write(-1, %q) = %d, %v; want -1, EBADF", msg, n, errno)
	}
}

func TestPtrCall(t *testing.T) {
	t.Run("strerror returns a string in C memory", func(t *testing.T) {
		p, errno := libcFunc(t, "strerror").Ptr(uintptr(syscall.EBADF), 0, 0, 0, 0, 0)
		if p == 0 || errno != 0 {
			t.Fatalf("strerror(EBADF) = %#x, %v; want a pointer, 0", p, errno)
		}
		var s []byte
		for i := uintptr(0); ; i++ {
			c := *dynlib.Pointer[byte](p + i)
			if c == 0 {
				break
			}
			s = append(s, c)
		}
		if want := syscall.EBADF.Error(); !strings.EqualFold(string(s), want) {
			t.Fatalf("strerror(EBADF) = %q, want %q", s, want)
		}
	})
	t.Run("opendir of a missing directory", func(t *testing.T) {
		path := cstring("/nonexistent/dynlib")
		p, errno := libcFunc(t, "opendir").Ptr(uintptr(unsafe.Pointer(&path[0])), 0, 0, 0, 0, 0)
		if p != 0 || errno != syscall.ENOENT {
			t.Fatalf("opendir(%q) = %#x, %v; want NULL, ENOENT", path[:len(path)-1], p, errno)
		}
	})
}

func fcntlSyscall(t *testing.T, fd int, cmd, arg uintptr) int {
	t.Helper()
	r, _, errno := syscall.Syscall(syscall.SYS_FCNTL, uintptr(fd), cmd, arg)
	if errno != 0 {
		t.Fatalf("fcntl(%d, %d, %#x): %v", fd, cmd, arg, errno)
	}
	return int(r)
}

func TestVariadicCall(t *testing.T) {
	r, _ := pipe(t)
	fcntl := libcFunc(t, "fcntl")
	flags, errno := fcntl.Int(uintptr(r), syscall.F_GETFL, 0, 0, 0, 0)
	if errno != 0 {
		t.Fatalf("fcntl(pipe, F_GETFL): %v", errno)
	}
	if want := fcntlSyscall(t, r, syscall.F_GETFL, 0); int(flags) != want {
		t.Fatalf("fcntl(pipe, F_GETFL) = %#x, syscall says %#x", flags, want)
	}
	if ret, errno := fcntl.Int(uintptr(r), syscall.F_SETFL, uintptr(flags)|syscall.O_NONBLOCK, 0, 0, 0); ret != 0 || errno != 0 {
		t.Fatalf("fcntl(pipe, F_SETFL, flags|O_NONBLOCK) = %d, %v; want 0, 0", ret, errno)
	}
	if got, want := fcntlSyscall(t, r, syscall.F_GETFL, 0), int(flags)|syscall.O_NONBLOCK; got != want {
		t.Fatalf("after F_SETFL syscall reads flags %#x, want %#x", got, want)
	}
}

func TestFuncAtCallsTheFunctionAtTheAddress(t *testing.T) {
	getpid := libcFunc(t, "getpid")
	pid, errno := dynlib.FuncAt(getpid.Addr()).Int(0, 0, 0, 0, 0, 0)
	if errno != 0 || int(pid) != os.Getpid() {
		t.Fatalf("FuncAt(getpid).Int() = %d, %v; want %d, 0", pid, errno, os.Getpid())
	}
}

func TestOpenUnknownLibrary(t *testing.T) {
	const name = "libdynlib-missing.so.0"
	lib, err := dynlib.Open(name)
	if lib != nil || err == nil {
		t.Fatalf("Open(%q) = %v, %v; want nil and an error", name, lib, err)
	}
	if !errors.Is(err, errors.ErrUnsupported) {
		t.Errorf("Open(%q): %v does not match errors.ErrUnsupported", name, err)
	}
	var e *dynlib.Error
	if !errors.As(err, &e) || e.Lib != name || e.Symbol != "" || e.Reason == "" {
		t.Fatalf("Open(%q) = %#v, want *dynlib.Error naming the library with the loader's reason", name, err)
	}
	if s := err.Error(); !strings.Contains(s, name) || !strings.Contains(s, "cannot open shared object file") {
		t.Errorf("Open(%q) = %q, want the library name and the loader's reason", name, s)
	}
}

func TestLookupUnknownSymbol(t *testing.T) {
	const name = "dynlib_no_such_function"
	_, err := openLibc(t).Lookup(name)
	var e *dynlib.Error
	if !errors.As(err, &e) || e.Lib != libc || e.Symbol != name || e.Reason == "" {
		t.Fatalf("Lookup(%q) = %#v, want *dynlib.Error naming the symbol with the loader's reason", name, err)
	}
	if s := err.Error(); !strings.Contains(s, name) {
		t.Errorf("Lookup(%q) = %q, want the symbol name", name, s)
	}
}

func TestOpenConcurrentlyReturnsOneLib(t *testing.T) {
	const name = "libm.so.6" // not opened by any other test
	libs := make([]*dynlib.Lib, 32)
	errs := make([]error, len(libs))
	var start, done sync.WaitGroup
	start.Add(1)
	for i := range libs {
		done.Add(1)
		go func() {
			defer done.Done()
			start.Wait()
			libs[i], errs[i] = dynlib.Open(name)
		}()
	}
	start.Done()
	done.Wait()
	for i := range libs {
		if errs[i] != nil {
			t.Fatal(errs[i])
		}
		if libs[i] != libs[0] {
			t.Fatalf("Open(%q) returned both %p and %p", name, libs[0], libs[i])
		}
	}
}
