//go:build linux && !android && (amd64 || arm64 || loong64 || ppc64le || riscv64 || s390x)

package verbs

import (
	"runtime"
	"syscall"
	"unsafe"

	"github.com/wzshiming/rdma/internal/dynlib"
	"github.com/wzshiming/rdma/internal/rdmaabi"
)

// PD is a protection domain (struct ibv_pd).
type PD struct {
	pd    unsafe.Pointer // struct ibv_pd *, opaque to the package
	owned bool
}

// AllocPD allocates a protection domain.
func (c *Context) AllocPD() (*PD, error) {
	f, err := load("ibv_alloc_pd")
	if err != nil {
		return nil, err
	}
	pd, errno := f.allocPD.Ptr(caddr(c.ctx), 0, 0, 0, 0, 0)
	if pd == 0 {
		return nil, opError("ibv_alloc_pd", errno)
	}
	return &PD{pd: unsafe.Pointer(dynlib.Pointer[byte](pd)), owned: true}, nil
}

// PDFromPointer wraps a struct ibv_pd pointer owned by someone else; Close
// does not deallocate it.
func PDFromPointer(p unsafe.Pointer) *PD { return &PD{pd: p} }

// Pointer returns the underlying struct ibv_pd pointer.
func (p *PD) Pointer() unsafe.Pointer { return p.pd }

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
	f, err := load("ibv_dealloc_pd")
	if err != nil {
		return err
	}
	if rc, _ := f.deallocPD.Int(uintptr(pd), 0, 0, 0, 0, 0); rc != 0 {
		return opErrorRC("ibv_dealloc_pd", rc)
	}
	return nil
}

// MR is a registered memory region (struct ibv_mr).
type MR struct {
	mr     *rdmaabi.IbvMr
	buf    []byte
	pinner runtime.Pinner
	mapped bool // buf comes from mmap and is unmapped on Close
	owned  bool
}

// regMR registers the memory at &b[0] with pd; b is pinned or off-heap.
func (p *PD) regMR(b []byte, access AccessFlags) (*rdmaabi.IbvMr, error) {
	f, err := load("ibv_reg_mr")
	if err != nil {
		return nil, err
	}
	mr, errno := f.regMR.Ptr(uintptr(p.pd), uintptr(unsafe.Pointer(&b[0])), uintptr(len(b)), cint(int32(access)), 0, 0)
	if mr == 0 {
		return nil, opError("ibv_reg_mr", errno)
	}
	return dynlib.Pointer[rdmaabi.IbvMr](mr), nil
}

// RegisterMemory registers the Go slice b for RDMA access. Its backing array
// is pinned with runtime.Pinner, and the caller must keep b alive, until Close.
func (p *PD) RegisterMemory(b []byte, access AccessFlags) (*MR, error) {
	if len(b) == 0 {
		return nil, &OpError{Op: "ibv_reg_mr", Err: syscall.EINVAL}
	}
	mr := &MR{buf: b, owned: true}
	mr.pinner.Pin(&b[0])
	cmr, err := p.regMR(b, access)
	if err != nil {
		mr.pinner.Unpin()
		return nil, err
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
	cmr, err := p.regMR(b, access)
	if err != nil {
		syscall.Munmap(b)
		return nil, err
	}
	return &MR{mr: cmr, buf: b, mapped: true, owned: true}, nil
}

// MRFromPointer wraps a struct ibv_mr pointer owned by someone else; Close
// does not deregister it.
func MRFromPointer(p unsafe.Pointer) *MR {
	mr := &MR{mr: (*rdmaabi.IbvMr)(p)}
	if mr.mr != nil {
		mr.buf = unsafe.Slice(dynlib.Pointer[byte](uintptr(mr.mr.Addr)), int(mr.mr.Length))
	}
	return mr
}

// Pointer returns the underlying struct ibv_mr pointer.
func (m *MR) Pointer() unsafe.Pointer { return unsafe.Pointer(m.mr) }

// Bytes returns the registered memory; nil after Close.
func (m *MR) Bytes() []byte { return m.buf }

// LKey returns the local key of the region.
func (m *MR) LKey() uint32 { return m.mr.Lkey }

// RKey returns the remote key of the region.
func (m *MR) RKey() uint32 { return m.mr.Rkey }

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
		f, err := load("ibv_dereg_mr")
		if err != nil {
			return err
		}
		if rc, _ := f.deregMR.Int(caddr(m.mr), 0, 0, 0, 0, 0); rc != 0 {
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
