// Package dynlib calls C functions of shared libraries without cgo.
//
// Open loads a library by its SONAME through the dynamic loader's own
// search, once per process, and Lib.Lookup resolves its exported functions;
// FuncAt wraps a function pointer read from C memory. Func.Int, Func.SSize
// and Func.Ptr call a function with up to six integer or pointer arguments
// and read its result as a C int, ssize_t or pointer; when the result says
// the call failed (-1, or NULL) they also hand back the errno it left,
// read on the thread that made the call, which the goroutine stays pinned
// to in between. C functions with a variable number of integer arguments
// are called the same way.
//
// Arguments are uintptr values: a C int is sign-extended to 64 bits before
// the call, a pointer into Go memory is converted in the call expression
// itself, as in f.Int(uintptr(unsafe.Pointer(p)), 0, 0, 0, 0, 0), which
// keeps p valid until the call returns. Pointer turns a C memory address
// back into a typed pointer.
//
// Only Linux on amd64, arm64, loong64, ppc64le, riscv64 and s390x has an
// implementation, with or without cgo; everywhere else Open fails with an
// error matching errors.ErrUnsupported. Nothing is loaded before the first
// Open, no library is ever unloaded and the package starts no goroutine.
package dynlib

import (
	"errors"
	"unsafe"
)

// Error reports a library the dynamic loader could not open or a function
// it lacks. It matches errors.ErrUnsupported.
type Error struct {
	Lib    string // SONAME of the library
	Symbol string // the function that is missing, "" when the library itself did not load
	Reason string // the dynamic loader's message
}

func (e *Error) Error() string {
	s := "dynlib: " + e.Lib
	if e.Symbol != "" {
		s += ": " + e.Symbol
	}
	return s + ": " + e.Reason
}

// Unwrap returns errors.ErrUnsupported.
func (e *Error) Unwrap() error { return errors.ErrUnsupported }

// Pointer returns a *T to the C memory at addr, which must not be the
// address of Go memory. It is the one place in the module that turns an
// integer address into a pointer.
func Pointer[T any](addr uintptr) *T {
	// Read through *unsafe.Pointer rather than convert: vet and checkptr
	// reject unsafe.Pointer(uintptr), rightly so for Go memory.
	return (*T)(*(*unsafe.Pointer)(unsafe.Pointer(&addr)))
}
