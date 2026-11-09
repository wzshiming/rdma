//go:build !linux || !cgo

package verbs

import (
	"errors"
	"unsafe"
)

func unsupported(op string) error { return &OpError{Op: op, Err: errors.ErrUnsupported} }

// Device is an RDMA device; libibverbs is unavailable on this platform.
type Device struct{}

// Devices returns the RDMA devices of the host.
func Devices() ([]*Device, error) { return nil, unsupported("ibv_get_device_list") }

// Name returns the kernel name of the device.
func (d *Device) Name() string { return "" }

// GUID returns the node GUID of the device.
func (d *Device) GUID() uint64 { return 0 }

// Open opens the device.
func (d *Device) Open() (*Context, error) { return nil, unsupported("ibv_open_device") }

// ForkInit wraps ibv_fork_init.
func ForkInit() error { return unsupported("ibv_fork_init") }

// Context is an open device; libibverbs is unavailable on this platform.
type Context struct{}

// ContextFromPointer wraps a struct ibv_context pointer without owning it.
func ContextFromPointer(p unsafe.Pointer) *Context { return &Context{} }

// Pointer returns the underlying struct ibv_context pointer.
func (c *Context) Pointer() unsafe.Pointer { return nil }

// Close closes the device.
func (c *Context) Close() error { return nil }

// QueryDevice returns the attributes of the device.
func (c *Context) QueryDevice() (*DeviceAttr, error) { return nil, unsupported("ibv_query_device") }

// QueryPort returns the attributes of a port.
func (c *Context) QueryPort(port uint8) (*PortAttr, error) {
	return nil, unsupported("ibv_query_port")
}

// GIDTable returns the populated GID table entries of a port.
func (c *Context) GIDTable(port uint8) ([]GIDEntry, error) {
	return nil, unsupported("ibv_query_gid_table")
}

// PD is a protection domain; libibverbs is unavailable on this platform.
type PD struct{}

// AllocPD allocates a protection domain.
func (c *Context) AllocPD() (*PD, error) { return nil, unsupported("ibv_alloc_pd") }

// PDFromPointer wraps a struct ibv_pd pointer without owning it.
func PDFromPointer(p unsafe.Pointer) *PD { return &PD{} }

// Pointer returns the underlying struct ibv_pd pointer.
func (p *PD) Pointer() unsafe.Pointer { return nil }

// Close deallocates the protection domain.
func (p *PD) Close() error { return nil }

// RegisterMemory registers the Go slice b for RDMA access.
func (p *PD) RegisterMemory(b []byte, access AccessFlags) (*MR, error) {
	return nil, unsupported("ibv_reg_mr")
}

// AllocMemory maps and registers page-aligned memory outside the Go heap.
func (p *PD) AllocMemory(size int, access AccessFlags) (*MR, error) {
	return nil, unsupported("ibv_reg_mr")
}

// MR is a registered memory region; libibverbs is unavailable on this platform.
type MR struct{}

// MRFromPointer wraps a struct ibv_mr pointer without owning it.
func MRFromPointer(p unsafe.Pointer) *MR { return &MR{} }

// Pointer returns the underlying struct ibv_mr pointer.
func (m *MR) Pointer() unsafe.Pointer { return nil }

// Bytes returns the registered memory.
func (m *MR) Bytes() []byte { return nil }

// LKey returns the local key of the region.
func (m *MR) LKey() uint32 { return 0 }

// RKey returns the remote key of the region.
func (m *MR) RKey() uint32 { return 0 }

// SGE builds a scatter/gather element for a sub-slice of Bytes.
func (m *MR) SGE(b []byte) SGE { return SGE{} }

// Close deregisters the region.
func (m *MR) Close() error { return nil }
