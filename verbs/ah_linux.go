//go:build linux && !android && (amd64 || arm64 || loong64 || ppc64le || riscv64 || s390x)

package verbs

import (
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
