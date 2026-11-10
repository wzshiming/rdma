//go:build linux && !android && (amd64 || arm64 || loong64 || ppc64le || riscv64 || s390x)

package verbs

import (
	"bytes"
	"encoding/binary"
	"sync"
	"sync/atomic"
	"syscall"
	"unsafe"

	"github.com/wzshiming/rdma/internal/dynlib"
	"github.com/wzshiming/rdma/internal/rdmaabi"
)

// libName is the SONAME the first use opens; only a test points it elsewhere.
var libName = "libibverbs.so.1"

// funcs holds the exported functions of libibverbs the package calls. The
// four verbs the header inlines (post_send, post_recv, poll_cq and
// req_notify_cq) come from the ops table of each device context instead.
type funcs struct {
	getDeviceList, freeDeviceList, getDeviceName, getDeviceGUID    dynlib.Func
	openDevice, closeDevice, forkInit                              dynlib.Func
	queryDevice, queryPort, queryGIDTable                          dynlib.Func
	createCompChannel, destroyCompChannel, getCQEvent, ackCQEvents dynlib.Func
	createCQ, destroyCQ, getAsyncEvent, ackAsyncEvent              dynlib.Func
	allocPD, deallocPD, regMR, deregMR                             dynlib.Func
	createQP, destroyQP, modifyQP, queryQP                         dynlib.Func
	wcStatusStr                                                    dynlib.Func
}

var (
	libMu sync.Mutex
	lib   atomic.Pointer[funcs] // set once the library is open and resolved
)

// load returns the functions of libibverbs, opening it on the first call. A
// failure is an *OpError of op that matches errors.ErrUnsupported; it is not
// remembered, so a later call tries again.
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
		return nil, &OpError{Op: op, Err: err}
	}
	f := new(funcs)
	for _, s := range []struct {
		name string
		fn   *dynlib.Func
	}{
		{"ibv_get_device_list", &f.getDeviceList},
		{"ibv_free_device_list", &f.freeDeviceList},
		{"ibv_get_device_name", &f.getDeviceName},
		{"ibv_get_device_guid", &f.getDeviceGUID},
		{"ibv_open_device", &f.openDevice},
		{"ibv_close_device", &f.closeDevice},
		{"ibv_fork_init", &f.forkInit},
		{"ibv_query_device", &f.queryDevice},
		{"ibv_query_port", &f.queryPort},
		{"_ibv_query_gid_table", &f.queryGIDTable},
		{"ibv_create_comp_channel", &f.createCompChannel},
		{"ibv_destroy_comp_channel", &f.destroyCompChannel},
		{"ibv_get_cq_event", &f.getCQEvent},
		{"ibv_ack_cq_events", &f.ackCQEvents},
		{"ibv_create_cq", &f.createCQ},
		{"ibv_destroy_cq", &f.destroyCQ},
		{"ibv_get_async_event", &f.getAsyncEvent},
		{"ibv_ack_async_event", &f.ackAsyncEvent},
		{"ibv_alloc_pd", &f.allocPD},
		{"ibv_dealloc_pd", &f.deallocPD},
		{"ibv_reg_mr", &f.regMR},
		{"ibv_dereg_mr", &f.deregMR},
		{"ibv_create_qp", &f.createQP},
		{"ibv_destroy_qp", &f.destroyQP},
		{"ibv_modify_qp", &f.modifyQP},
		{"ibv_query_qp", &f.queryQP},
		{"ibv_wc_status_str", &f.wcStatusStr},
	} {
		if *s.fn, err = l.Lookup(s.name); err != nil {
			return nil, &OpError{Op: op, Err: err}
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
	return &OpError{Op: op, Err: errno}
}

// opErrorRC reports a call that returned the errno value (some providers
// return its negative).
func opErrorRC(op string, rc int32) error {
	if rc < 0 {
		rc = -rc
	}
	if rc == 0 {
		rc = int32(syscall.EIO)
	}
	return &OpError{Op: op, Err: syscall.Errno(rc)}
}

// cint passes n as a C int: sign-extended to the register, as the ABIs want.
func cint(n int32) uintptr { return uintptr(int64(n)) }

// caddr passes a pointer to C memory as an argument. Go memory is passed
// through a conversion in the call expression itself instead, which keeps
// it alive for the call; caddr does not.
func caddr[T any](p *T) uintptr { return uintptr(unsafe.Pointer(p)) }

// contextOps returns the ops table of the device context at addr.
func contextOps(addr uint64) *rdmaabi.IbvContextOps {
	return &dynlib.Pointer[rdmaabi.IbvContext](uintptr(addr)).Ops
}

// goString copies the NUL-terminated C string at addr.
func goString(addr uintptr) string {
	if addr == 0 {
		return ""
	}
	n := 0
	for *dynlib.Pointer[byte](addr + uintptr(n)) != 0 {
		n++
	}
	return string(unsafe.Slice(dynlib.Pointer[byte](addr), n))
}

// cString returns the NUL-terminated string at the front of b.
func cString(b []byte) string {
	if i := bytes.IndexByte(b, 0); i >= 0 {
		b = b[:i]
	}
	return string(b)
}

// guid converts a __be64 as stored in memory into its value.
func guid(b [8]byte) uint64 { return binary.BigEndian.Uint64(b[:]) }

// be64 converts a __be64 as loaded into a register into its value.
func be64(v uint64) uint64 {
	var b [8]byte
	binary.NativeEndian.PutUint64(b[:], v)
	return guid(b)
}
