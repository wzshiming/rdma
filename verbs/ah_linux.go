//go:build linux && !android && (amd64 || arm64 || loong64 || ppc64le || riscv64 || s390x)

package verbs

import (
	"fmt"
	"syscall"
	"unsafe"

	"github.com/wzshiming/rdma/internal/dynlib"
)

// AH is an address handle (struct ibv_ah): the destination of the datagrams
// a UD queue pair sends.
type AH struct {
	ah    unsafe.Pointer // struct ibv_ah *, opaque to the package
	owned bool
}

// CreateAH creates an address handle for the destination attr describes, an
// address vector as a connected queue pair takes one in QPAttr.AH.
func (p *PD) CreateAH(attr *AHAttr) (*AH, error) {
	f, err := load("ibv_create_ah")
	if err != nil {
		return nil, err
	}
	ca := ahToC(*attr)
	ah, errno := f.createAH.Ptr(uintptr(p.pd), uintptr(unsafe.Pointer(&ca)), 0, 0, 0, 0)
	if ah == 0 {
		return nil, opError("ibv_create_ah", errno)
	}
	return &AH{ah: unsafe.Pointer(dynlib.Pointer[byte](ah)), owned: true}, nil
}

// grhSize is the size of the global routing header at the front of a UD
// receive buffer.
const grhSize = 40

// CreateAHFromWC creates an address handle for the sender of a datagram, as
// ibv_create_ah_from_wc does, from the receive completion wc, grh, the 40
// bytes at the front of its receive buffer, and port, the port that received
// it.
func (p *PD) CreateAHFromWC(wc *WC, grh []byte, port uint8) (*AH, error) {
	const op = "ibv_create_ah_from_wc"
	if len(grh) < grhSize {
		return nil, &OpError{Op: op, Err: fmt.Errorf("%d bytes of global routing header, want %d: %w", len(grh), grhSize, syscall.EINVAL)}
	}
	f, err := load(op)
	if err != nil {
		return nil, err
	}
	cw := wcToC(wc)
	ah, errno := f.createAHFromWC.Ptr(uintptr(p.pd), uintptr(unsafe.Pointer(&cw)), uintptr(unsafe.Pointer(&grh[0])), uintptr(port), 0, 0)
	if ah == 0 {
		return nil, opError(op, errno)
	}
	return &AH{ah: unsafe.Pointer(dynlib.Pointer[byte](ah)), owned: true}, nil
}

// AHFromPointer wraps a struct ibv_ah pointer owned by someone else; Close
// does not destroy it.
func AHFromPointer(p unsafe.Pointer) *AH { return &AH{ah: p} }

// Pointer returns the underlying struct ibv_ah pointer.
func (a *AH) Pointer() unsafe.Pointer { return a.ah }

// Close destroys the address handle. A second Close is a no-op.
func (a *AH) Close() error {
	if a.ah == nil {
		return nil
	}
	if a.owned {
		f, err := load("ibv_destroy_ah")
		if err != nil {
			return err
		}
		if rc, _ := f.destroyAH.Int(uintptr(a.ah), 0, 0, 0, 0, 0); rc != 0 {
			return opErrorRC("ibv_destroy_ah", rc)
		}
	}
	a.ah = nil
	return nil
}
