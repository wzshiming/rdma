//go:build linux && !android && (amd64 || arm64 || loong64 || ppc64le || riscv64 || s390x)

package dynlib

import (
	"runtime"
	"strings"
	"sync"
	"sync/atomic"
	"syscall"

	"github.com/ebitengine/purego"
)

// Lib is a shared library loaded into the process, never to be unloaded.
type Lib struct {
	name   string
	handle uintptr
}

var (
	mu   sync.Mutex
	libs = map[string]*Lib{}

	// errnoLocation is the address of libc's __errno_location, resolved by
	// the first Open, before any call can fail.
	errnoLocation atomic.Uintptr
)

// Open loads the shared library whose SONAME is name through the dynamic
// loader's own search and returns the one *Lib of that name for the rest of
// the process. It fails with an *Error, which matches errors.ErrUnsupported,
// when the loader cannot open the library.
func Open(name string) (*Lib, error) {
	mu.Lock()
	defer mu.Unlock()
	if lib, ok := libs[name]; ok {
		return lib, nil
	}
	if errnoLocation.Load() == 0 {
		// libc is in the global scope of every process purego links.
		addr, err := purego.Dlsym(purego.RTLD_DEFAULT, "__errno_location")
		if err != nil {
			return nil, loaderError(name, "__errno_location", err)
		}
		errnoLocation.Store(addr)
	}
	handle, err := purego.Dlopen(name, purego.RTLD_NOW)
	if err != nil {
		return nil, loaderError(name, "", err)
	}
	lib := &Lib{name: name, handle: handle}
	libs[name] = lib
	return lib, nil
}

// loaderError wraps the loader's message, which may already begin with the
// library's name.
func loaderError(lib, symbol string, err error) *Error {
	return &Error{Lib: lib, Symbol: symbol, Reason: strings.TrimPrefix(err.Error(), lib+": ")}
}

// Lookup resolves the exported function name, in its default symbol
// version. It fails with an *Error naming the function when the library
// lacks it.
func (l *Lib) Lookup(name string) (Func, error) {
	addr, err := purego.Dlsym(l.handle, name)
	if err != nil {
		return Func{}, loaderError(l.name, name, err)
	}
	return Func{addr: addr}, nil
}

// Func is a C function.
type Func struct{ addr uintptr }

// FuncAt returns the function at addr, a function pointer read from C
// memory that a library opened by Open handed out.
func FuncAt(addr uintptr) Func { return Func{addr: addr} }

// Addr returns the address of the function.
func (f Func) Addr() uintptr { return f.addr }

// Int calls f with the six arguments, zero where the function takes fewer,
// and returns its result as a C int; when that is -1 it also returns the
// errno the call left.
//
//go:uintptrescapes
func (f Func) Int(a1, a2, a3, a4, a5, a6 uintptr) (int32, syscall.Errno) {
	runtime.LockOSThread()
	defer runtime.UnlockOSThread()
	r := int32(uint32(f.call(a1, a2, a3, a4, a5, a6)))
	if r != -1 {
		return r, 0
	}
	return r, lastErrno()
}

// SSize calls f like Int does and returns its result as a C ssize_t; when
// that is -1 it also returns the errno the call left.
//
//go:uintptrescapes
func (f Func) SSize(a1, a2, a3, a4, a5, a6 uintptr) (int, syscall.Errno) {
	runtime.LockOSThread()
	defer runtime.UnlockOSThread()
	r := int(f.call(a1, a2, a3, a4, a5, a6))
	if r != -1 {
		return r, 0
	}
	return r, lastErrno()
}

// Ptr calls f like Int does and returns its result as a C pointer; when
// that is NULL it also returns the errno the call left.
//
//go:uintptrescapes
func (f Func) Ptr(a1, a2, a3, a4, a5, a6 uintptr) (uintptr, syscall.Errno) {
	runtime.LockOSThread()
	defer runtime.UnlockOSThread()
	r := f.call(a1, a2, a3, a4, a5, a6)
	if r != 0 {
		return r, 0
	}
	return 0, lastErrno()
}

// argBlocks holds the argument slices handed to purego: SyscallN's variadic
// slice escapes to the heap, so a literal would cost an allocation per call.
var argBlocks = sync.Pool{New: func() any { return new([6]uintptr) }}

func (f Func) call(a1, a2, a3, a4, a5, a6 uintptr) uintptr {
	b := argBlocks.Get().(*[6]uintptr)
	*b = [6]uintptr{a1, a2, a3, a4, a5, a6}
	r1, _, _ := purego.SyscallN(f.addr, b[:]...)
	argBlocks.Put(b)
	return r1
}

// testHookBeforeErrno runs between a failed call and the read of its errno;
// a test sets it to give the processor away right there.
var testHookBeforeErrno func()

// lastErrno returns the errno of the current thread, which the caller keeps
// pinned to its goroutine since the call that set it.
func lastErrno() syscall.Errno {
	if testHookBeforeErrno != nil {
		testHookBeforeErrno()
	}
	p, _, _ := purego.SyscallN(errnoLocation.Load())
	return syscall.Errno(*Pointer[int32](p))
}
