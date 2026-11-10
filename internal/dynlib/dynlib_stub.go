//go:build !linux || android || !(amd64 || arm64 || loong64 || ppc64le || riscv64 || s390x)

package dynlib

import "runtime"

// Lib is a shared library; none can be loaded on this platform.
type Lib struct{}

// Open fails with an *Error matching errors.ErrUnsupported: this platform
// has no implementation.
func Open(name string) (*Lib, error) {
	return nil, &Error{Lib: name, Reason: "shared libraries cannot be called on " + runtime.GOOS + "/" + runtime.GOARCH}
}
