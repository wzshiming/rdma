//go:build linux && cgo

package verbs

/*
#include <infiniband/verbs.h>

// ibv_reg_mr is a macro in recent rdma-core; cgo needs a function.
static struct ibv_mr *verbs_reg_mr(struct ibv_pd *pd, void *addr, size_t length, unsigned int access)
{
	return ibv_reg_mr(pd, addr, length, access);
}
*/
import "C"

import (
	"runtime"
	"syscall"
	"unsafe"
)

// PD is a protection domain (struct ibv_pd).
type PD struct {
	pd    *C.struct_ibv_pd
	owned bool
}

// AllocPD allocates a protection domain.
func (c *Context) AllocPD() (*PD, error) {
	pd, err := C.ibv_alloc_pd(c.ctx)
	if pd == nil {
		return nil, opError("ibv_alloc_pd", err)
	}
	return &PD{pd: pd, owned: true}, nil
}

// PDFromPointer wraps a struct ibv_pd pointer owned by someone else; Close
// does not deallocate it.
func PDFromPointer(p unsafe.Pointer) *PD { return &PD{pd: (*C.struct_ibv_pd)(p)} }

// Pointer returns the underlying struct ibv_pd pointer.
func (p *PD) Pointer() unsafe.Pointer { return unsafe.Pointer(p.pd) }

// Close deallocates the protection domain. A second Close is a no-op.
func (p *PD) Close() error {
	pd := p.pd
	if pd == nil {
		return nil
	}
	p.pd = nil
	if !p.owned {
		return nil
	}
	if rc := C.ibv_dealloc_pd(pd); rc != 0 {
		return opErrorRC("ibv_dealloc_pd", rc)
	}
	return nil
}

// MR is a registered memory region (struct ibv_mr).
type MR struct {
	mr     *C.struct_ibv_mr
	buf    []byte
	pinner runtime.Pinner
	mapped bool // buf comes from mmap and is unmapped on Close
	owned  bool
}

// RegisterMemory registers the Go slice b for RDMA access. Its backing array
// is pinned with runtime.Pinner, and the caller must keep b alive, until Close.
func (p *PD) RegisterMemory(b []byte, access AccessFlags) (*MR, error) {
	if len(b) == 0 {
		return nil, &OpError{Op: "ibv_reg_mr", Err: syscall.EINVAL}
	}
	mr := &MR{buf: b, owned: true}
	mr.pinner.Pin(&b[0])
	cmr, err := C.verbs_reg_mr(p.pd, unsafe.Pointer(&b[0]), C.size_t(len(b)), C.uint(access))
	if cmr == nil {
		mr.pinner.Unpin()
		return nil, opError("ibv_reg_mr", err)
	}
	mr.mr = cmr
	return mr, nil
}

// AllocMemory maps size bytes of page-aligned anonymous memory outside the Go
// heap and registers them; Close unmaps them.
func (p *PD) AllocMemory(size int, access AccessFlags) (*MR, error) {
	if size <= 0 {
		return nil, &OpError{Op: "mmap", Err: syscall.EINVAL}
	}
	b, err := syscall.Mmap(-1, 0, size, syscall.PROT_READ|syscall.PROT_WRITE, syscall.MAP_ANON|syscall.MAP_PRIVATE)
	if err != nil {
		return nil, &OpError{Op: "mmap", Err: err}
	}
	cmr, err := C.verbs_reg_mr(p.pd, unsafe.Pointer(&b[0]), C.size_t(size), C.uint(access))
	if cmr == nil {
		syscall.Munmap(b)
		return nil, opError("ibv_reg_mr", err)
	}
	return &MR{mr: cmr, buf: b, mapped: true, owned: true}, nil
}

// MRFromPointer wraps a struct ibv_mr pointer owned by someone else; Close
// does not deregister it.
func MRFromPointer(p unsafe.Pointer) *MR {
	mr := &MR{mr: (*C.struct_ibv_mr)(p)}
	if mr.mr != nil {
		mr.buf = unsafe.Slice((*byte)(mr.mr.addr), int(mr.mr.length))
	}
	return mr
}

// Pointer returns the underlying struct ibv_mr pointer.
func (m *MR) Pointer() unsafe.Pointer { return unsafe.Pointer(m.mr) }

// Bytes returns the registered memory; nil after Close.
func (m *MR) Bytes() []byte { return m.buf }

// LKey returns the local key of the region.
func (m *MR) LKey() uint32 { return uint32(m.mr.lkey) }

// RKey returns the remote key of the region.
func (m *MR) RKey() uint32 { return uint32(m.mr.rkey) }

// SGE builds a scatter/gather element for b, which must be a sub-slice of
// Bytes; it panics otherwise.
func (m *MR) SGE(b []byte) SGE {
	base := uintptr(unsafe.Pointer(unsafe.SliceData(m.buf)))
	addr := uintptr(unsafe.Pointer(unsafe.SliceData(b)))
	if addr < base || addr+uintptr(len(b)) > base+uintptr(len(m.buf)) {
		panic("verbs: MR.SGE: slice is not within the registered region")
	}
	return SGE{Addr: uint64(addr), Length: uint32(len(b)), LKey: m.LKey()}
}

// Close deregisters the region, unpins or unmaps its memory and makes Bytes
// return nil. A second Close is a no-op.
func (m *MR) Close() error {
	if m.mr == nil {
		return nil
	}
	if m.owned {
		if rc := C.ibv_dereg_mr(m.mr); rc != 0 {
			return opErrorRC("ibv_dereg_mr", rc)
		}
	}
	m.mr = nil
	m.pinner.Unpin()
	if m.mapped {
		syscall.Munmap(m.buf)
	}
	m.buf = nil
	return nil
}
