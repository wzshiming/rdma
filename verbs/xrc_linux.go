//go:build linux && !android && (amd64 || arm64 || loong64 || ppc64le || riscv64 || s390x)

package verbs

import (
	"os"
	"syscall"
	"unsafe"

	"github.com/wzshiming/rdma/internal/dynlib"
	"github.com/wzshiming/rdma/internal/rdmaabi"
)

// XRCD is an XRC domain (struct ibv_xrcd): the XRC SRQs and XRC receive
// queue pairs created in it share it, as do the processes that open it
// through the same file.
type XRCD struct {
	xrcd  *rdmaabi.IbvXrcd
	owned bool
}

// OpenXRCD opens an XRC domain on the device (ibv_open_xrcd). With fd -1 it
// is a new domain of the process's own; with the descriptor of an open file
// it is the domain of that file, which every process that opens a domain
// through the same file shares: oflags O_CREAT creates it when there is none
// yet, and O_EXCL refuses one that exists. A device or library without XRC
// reports syscall.EOPNOTSUPP, which matches errors.ErrUnsupported.
func (c *Context) OpenXRCD(fd, oflags int) (*XRCD, error) {
	if c.ctx == nil {
		return nil, &OpError{Op: "ibv_open_xrcd", Err: os.ErrClosed}
	}
	open := verbsOp(caddr(c.ctx), unsafe.Offsetof(rdmaabi.VerbsContext{}.Open_xrcd))
	if open.Addr() == 0 {
		return nil, &OpError{Op: "ibv_open_xrcd", Err: syscall.EOPNOTSUPP}
	}
	attr := xrcdInitAttrToC(fd, oflags)
	xrcd, errno := open.Ptr(caddr(c.ctx), uintptr(unsafe.Pointer(&attr)), 0, 0, 0, 0)
	if xrcd == 0 {
		return nil, opError("ibv_open_xrcd", errno)
	}
	return &XRCD{xrcd: dynlib.Pointer[rdmaabi.IbvXrcd](xrcd), owned: true}, nil
}

// xrcdInitAttrToC returns the struct ibv_xrcd_init_attr of fd and oflags,
// with the comp_mask bits of both (IBV_XRCD_INIT_ATTR_FD and _OFLAGS), which
// libibverbs requires.
func xrcdInitAttrToC(fd, oflags int) rdmaabi.IbvXrcdInitAttr {
	return rdmaabi.IbvXrcdInitAttr{Comp_mask: 1<<0 | 1<<1, Fd: int32(fd), Oflags: int32(oflags)}
}

// XRCDFromPointer wraps a struct ibv_xrcd pointer owned by someone else;
// Close does not close it.
func XRCDFromPointer(p unsafe.Pointer) *XRCD { return &XRCD{xrcd: (*rdmaabi.IbvXrcd)(p)} }

// Pointer returns the underlying struct ibv_xrcd pointer.
func (x *XRCD) Pointer() unsafe.Pointer { return unsafe.Pointer(x.xrcd) }

// Close closes the XRC domain (ibv_close_xrcd). A second Close is a no-op.
func (x *XRCD) Close() error {
	xrcd := x.xrcd
	if xrcd == nil {
		return nil
	}
	if x.owned {
		closeXRCD := verbsOp(uintptr(xrcd.Context), unsafe.Offsetof(rdmaabi.VerbsContext{}.Close_xrcd))
		if closeXRCD.Addr() == 0 {
			return &OpError{Op: "ibv_close_xrcd", Err: syscall.EOPNOTSUPP}
		}
		if rc, _ := closeXRCD.Int(caddr(xrcd), 0, 0, 0, 0, 0); rc != 0 {
			return opErrorRC("ibv_close_xrcd", rc)
		}
	}
	x.xrcd = nil
	return nil
}
