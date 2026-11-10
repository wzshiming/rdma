//go:build linux && !android && (amd64 || arm64 || loong64 || ppc64le || riscv64 || s390x)

package cm

import (
	"os"
	"sync"
	"sync/atomic"
	"syscall"
	"unsafe"

	"github.com/wzshiming/rdma/internal/dynlib"
	"github.com/wzshiming/rdma/verbs"
)

// libName is the SONAME the first use opens; only a test points it elsewhere.
var libName = "librdmacm.so.1"

// funcs holds the exported functions of librdmacm the package calls.
// rdma_get_local_addr and rdma_get_peer_addr are header inlines, so the
// addresses are read from the route of the identifier instead.
type funcs struct {
	createEventChannel, destroyEventChannel dynlib.Func
	createID, destroyID, migrateID          dynlib.Func
	getCMEvent, ackCMEvent                  dynlib.Func
	resolveAddr, resolveRoute               dynlib.Func
	bindAddr, listen                        dynlib.Func
	createQP, destroyQP                     dynlib.Func
	connect, accept, reject, disconnect     dynlib.Func
}

var (
	libMu sync.Mutex
	lib   atomic.Pointer[funcs] // set once the library is open and resolved
)

// load returns the functions of librdmacm, opening it on the first call. A
// failure is a *verbs.OpError of op that matches errors.ErrUnsupported and
// names the library; it is not remembered, so a later call tries again.
func load(op string) (*funcs, error) {
	if f := lib.Load(); f != nil {
		return f, nil
	}
	libMu.Lock()
	defer libMu.Unlock()
	if f := lib.Load(); f != nil {
		return f, nil
	}
	l, err := dynlib.Open(libName)
	if err != nil {
		return nil, &verbs.OpError{Op: op, Err: err}
	}
	f := new(funcs)
	for _, s := range []struct {
		name string
		fn   *dynlib.Func
	}{
		{"rdma_create_event_channel", &f.createEventChannel},
		{"rdma_destroy_event_channel", &f.destroyEventChannel},
		{"rdma_create_id", &f.createID},
		{"rdma_destroy_id", &f.destroyID},
		{"rdma_migrate_id", &f.migrateID},
		{"rdma_get_cm_event", &f.getCMEvent},
		{"rdma_ack_cm_event", &f.ackCMEvent},
		{"rdma_resolve_addr", &f.resolveAddr},
		{"rdma_resolve_route", &f.resolveRoute},
		{"rdma_bind_addr", &f.bindAddr},
		{"rdma_listen", &f.listen},
		{"rdma_create_qp", &f.createQP},
		{"rdma_destroy_qp", &f.destroyQP},
		{"rdma_connect", &f.connect},
		{"rdma_accept", &f.accept},
		{"rdma_reject", &f.reject},
		{"rdma_disconnect", &f.disconnect},
	} {
		if *s.fn, err = l.Lookup(s.name); err != nil {
			return nil, &verbs.OpError{Op: op, Err: err}
		}
	}
	lib.Store(f)
	return f, nil
}

// opError reports a call that failed with errno set.
func opError(op string, errno syscall.Errno) error {
	if errno == 0 {
		errno = syscall.EIO
	}
	return &verbs.OpError{Op: op, Err: errno}
}

func errClosed(op string) error { return &verbs.OpError{Op: op, Err: os.ErrClosed} }

// caddr passes a pointer to C memory as an argument. Go memory is passed
// through a conversion in the call expression itself instead, which keeps
// it alive for the call; caddr does not.
func caddr[T any](p *T) uintptr { return uintptr(unsafe.Pointer(p)) }

// cpointer returns the C memory a pointer field of an rdma_cm structure
// addresses, for the verbs FromPointer constructors; NULL gives nil.
func cpointer(addr uint64) unsafe.Pointer { return unsafe.Pointer(dynlib.Pointer[byte](uintptr(addr))) }
