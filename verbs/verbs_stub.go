//go:build !linux || android || !(amd64 || arm64 || loong64 || ppc64le || riscv64 || s390x)

package verbs

import (
	"context"
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

// CompChannel is a completion event channel; libibverbs is unavailable on
// this platform.
type CompChannel struct{}

// CreateCompChannel creates a completion event channel.
func (c *Context) CreateCompChannel() (*CompChannel, error) {
	return nil, unsupported("ibv_create_comp_channel")
}

// CompChannelFromPointer wraps a struct ibv_comp_channel pointer without owning it.
func CompChannelFromPointer(p unsafe.Pointer) *CompChannel { return &CompChannel{} }

// Pointer returns the underlying struct ibv_comp_channel pointer.
func (c *CompChannel) Pointer() unsafe.Pointer { return nil }

// Wait waits for the next completion event.
func (c *CompChannel) Wait(ctx context.Context) (*CQ, error) {
	return nil, unsupported("ibv_get_cq_event")
}

// Close destroys the channel.
func (c *CompChannel) Close() error { return nil }

// WaitAsyncEvent waits for the next asynchronous event of the device.
func (c *Context) WaitAsyncEvent(ctx context.Context) (*AsyncEvent, error) {
	return nil, unsupported("ibv_get_async_event")
}

// CQ is a completion queue; libibverbs is unavailable on this platform.
type CQ struct{}

// CreateCQ creates a completion queue.
func (c *Context) CreateCQ(cqe int, ch *CompChannel) (*CQ, error) {
	return nil, unsupported("ibv_create_cq")
}

// CQFromPointer wraps a struct ibv_cq pointer without owning it.
func CQFromPointer(p unsafe.Pointer) *CQ { return &CQ{} }

// Pointer returns the underlying struct ibv_cq pointer.
func (q *CQ) Pointer() unsafe.Pointer { return nil }

// Poll removes completions from the queue.
func (q *CQ) Poll(wcs []WC) (int, error) { return 0, unsupported("ibv_poll_cq") }

// RequestNotify arms the completion channel for the next completion.
func (q *CQ) RequestNotify(solicitedOnly bool) error { return unsupported("ibv_req_notify_cq") }

// Close destroys the completion queue.
func (q *CQ) Close() error { return nil }

// QP is a queue pair; libibverbs is unavailable on this platform.
type QP struct{}

// CreateQP creates a queue pair.
func (p *PD) CreateQP(attr *QPInitAttr) (*QP, error) { return nil, unsupported("ibv_create_qp") }

// QPFromPointer wraps a struct ibv_qp pointer without owning it.
func QPFromPointer(p unsafe.Pointer) *QP { return &QP{} }

// Pointer returns the underlying struct ibv_qp pointer.
func (q *QP) Pointer() unsafe.Pointer { return nil }

// Num returns the queue pair number.
func (q *QP) Num() uint32 { return 0 }

// Type returns the transport service type.
func (q *QP) Type() QPType { return 0 }

// Modify applies the attr fields selected by mask.
func (q *QP) Modify(attr *QPAttr, mask QPAttrMask) error { return unsupported("ibv_modify_qp") }

// Query returns the attributes selected by mask.
func (q *QP) Query(mask QPAttrMask) (*QPAttr, error) { return nil, unsupported("ibv_query_qp") }

// PostSend posts send work requests.
func (q *QP) PostSend(wrs ...SendWR) error { return unsupported("ibv_post_send") }

// PostRecv posts receive work requests.
func (q *QP) PostRecv(wrs ...RecvWR) error { return unsupported("ibv_post_recv") }

// Close destroys the queue pair.
func (q *QP) Close() error { return nil }
