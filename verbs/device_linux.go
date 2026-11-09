//go:build linux && cgo

package verbs

/*
#cgo LDFLAGS: -libverbs
#include <errno.h>
#include <stdlib.h>
#include <infiniband/verbs.h>

// ibv_query_port is a macro in recent rdma-core; cgo needs a function.
static int verbs_query_port(struct ibv_context *ctx, uint8_t port, struct ibv_port_attr *attr)
{
	return ibv_query_port(ctx, port, attr);
}
*/
import "C"

import (
	"encoding/binary"
	"runtime"
	"syscall"
	"unsafe"
)

// opError reports a call that failed with errno set, as read by cgo into err.
func opError(op string, err error) error {
	errno, ok := err.(syscall.Errno)
	if !ok || errno == 0 {
		errno = syscall.EIO
	}
	return &OpError{Op: op, Err: errno}
}

// opErrorRC reports a call that returned the errno value (some providers
// return its negative).
func opErrorRC(op string, rc C.int) error {
	if rc < 0 {
		rc = -rc
	}
	if rc == 0 {
		rc = C.int(syscall.EIO)
	}
	return &OpError{Op: op, Err: syscall.Errno(rc)}
}

// be64 converts a __be64 value as loaded from memory into its logical value.
func be64(v uint64) uint64 {
	return binary.BigEndian.Uint64((*[8]byte)(unsafe.Pointer(&v))[:])
}

// deviceList keeps the array of ibv_get_device_list alive while any Device
// from it is reachable; opened devices stay valid on their own.
type deviceList struct {
	list **C.struct_ibv_device
}

// Device is an RDMA device returned by Devices.
type Device struct {
	dev  *C.struct_ibv_device
	list *deviceList
	name string
	guid uint64
}

// Devices returns the RDMA devices of the host.
func Devices() ([]*Device, error) {
	var n C.int
	list, err := C.ibv_get_device_list(&n)
	if list == nil {
		return nil, opError("ibv_get_device_list", err)
	}
	dl := &deviceList{list: list}
	runtime.AddCleanup(dl, func(l **C.struct_ibv_device) { C.ibv_free_device_list(l) }, list)
	devs := make([]*Device, 0, int(n))
	for _, d := range unsafe.Slice(list, int(n)) {
		devs = append(devs, &Device{
			dev:  d,
			list: dl,
			name: C.GoString(C.ibv_get_device_name(d)),
			guid: be64(uint64(C.ibv_get_device_guid(d))),
		})
	}
	return devs, nil
}

// Name returns the kernel name of the device, such as "rxe0" or "mlx5_0".
func (d *Device) Name() string { return d.name }

// GUID returns the node GUID of the device.
func (d *Device) GUID() uint64 { return d.guid }

// Open opens the device.
func (d *Device) Open() (*Context, error) {
	ctx, err := C.ibv_open_device(d.dev)
	runtime.KeepAlive(d)
	if ctx == nil {
		return nil, opError("ibv_open_device", err)
	}
	return &Context{ctx: ctx, owned: true}, nil
}

// ForkInit wraps ibv_fork_init, which makes registered memory safe across
// fork. Kernels 5.12 and newer do not need it; the package never calls it.
func ForkInit() error {
	if rc := C.ibv_fork_init(); rc != 0 {
		return opErrorRC("ibv_fork_init", rc)
	}
	return nil
}

// Context is an open device (struct ibv_context).
type Context struct {
	ctx   *C.struct_ibv_context
	owned bool
	async fdWaiter
}

// ContextFromPointer wraps a struct ibv_context pointer owned by someone else,
// such as the verbs context of an rdma_cm id. Close does not close the device.
func ContextFromPointer(p unsafe.Pointer) *Context {
	return &Context{ctx: (*C.struct_ibv_context)(p)}
}

// Pointer returns the underlying struct ibv_context pointer.
func (c *Context) Pointer() unsafe.Pointer { return unsafe.Pointer(c.ctx) }

// Close closes the device and wakes a pending WaitAsyncEvent with an error.
// A second Close is a no-op.
func (c *Context) Close() error {
	ctx := c.ctx
	if ctx == nil {
		return nil
	}
	c.async.close()
	c.ctx = nil
	if !c.owned {
		return nil
	}
	if rc, err := C.ibv_close_device(ctx); rc != 0 {
		return opError("ibv_close_device", err)
	}
	return nil
}

// QueryDevice returns the attributes and limits of the device.
func (c *Context) QueryDevice() (*DeviceAttr, error) {
	var a C.struct_ibv_device_attr
	if rc := C.ibv_query_device(c.ctx, &a); rc != 0 {
		return nil, opErrorRC("ibv_query_device", rc)
	}
	return &DeviceAttr{
		FWVer:           C.GoString(&a.fw_ver[0]),
		NodeGUID:        be64(uint64(a.node_guid)),
		SysImageGUID:    be64(uint64(a.sys_image_guid)),
		MaxMRSize:       uint64(a.max_mr_size),
		PageSizeCap:     uint64(a.page_size_cap),
		VendorID:        uint32(a.vendor_id),
		VendorPartID:    uint32(a.vendor_part_id),
		HWVer:           uint32(a.hw_ver),
		MaxQP:           int(a.max_qp),
		MaxQPWR:         int(a.max_qp_wr),
		DeviceCapFlags:  uint32(a.device_cap_flags),
		MaxSGE:          int(a.max_sge),
		MaxSGERD:        int(a.max_sge_rd),
		MaxCQ:           int(a.max_cq),
		MaxCQE:          int(a.max_cqe),
		MaxMR:           int(a.max_mr),
		MaxPD:           int(a.max_pd),
		MaxQPRDAtom:     int(a.max_qp_rd_atom),
		MaxResRDAtom:    int(a.max_res_rd_atom),
		MaxQPInitRDAtom: int(a.max_qp_init_rd_atom),
		MaxMW:           int(a.max_mw),
		MaxAH:           int(a.max_ah),
		MaxSRQ:          int(a.max_srq),
		MaxSRQWR:        int(a.max_srq_wr),
		MaxSRQSGE:       int(a.max_srq_sge),
		MaxPkeys:        uint16(a.max_pkeys),
		LocalCAAckDelay: uint8(a.local_ca_ack_delay),
		PhysPortCnt:     uint8(a.phys_port_cnt),
	}, nil
}

// QueryPort returns the attributes of a port; ports are numbered from 1.
func (c *Context) QueryPort(port uint8) (*PortAttr, error) {
	var a C.struct_ibv_port_attr
	if rc := C.verbs_query_port(c.ctx, C.uint8_t(port), &a); rc != 0 {
		return nil, opErrorRC("ibv_query_port", rc)
	}
	return &PortAttr{
		State:         PortState(a.state),
		MaxMTU:        MTU(a.max_mtu),
		ActiveMTU:     MTU(a.active_mtu),
		GIDTableLen:   int(a.gid_tbl_len),
		PortCapFlags:  uint32(a.port_cap_flags),
		MaxMsgSize:    uint32(a.max_msg_sz),
		BadPkeyCntr:   uint32(a.bad_pkey_cntr),
		QKeyViolCntr:  uint32(a.qkey_viol_cntr),
		PkeyTableLen:  uint16(a.pkey_tbl_len),
		LID:           uint16(a.lid),
		SMLID:         uint16(a.sm_lid),
		LMC:           uint8(a.lmc),
		MaxVLNum:      uint8(a.max_vl_num),
		SMSL:          uint8(a.sm_sl),
		SubnetTimeout: uint8(a.subnet_timeout),
		InitTypeReply: uint8(a.init_type_reply),
		ActiveWidth:   uint8(a.active_width),
		ActiveSpeed:   uint8(a.active_speed),
		PhysState:     uint8(a.phys_state),
		LinkLayer:     LinkLayer(a.link_layer),
	}, nil
}

// GIDTable returns the populated GID table entries of a port with their index
// and type (IB, RoCE v1 or RoCE v2).
func (c *Context) GIDTable(port uint8) ([]GIDEntry, error) {
	dev, err := c.QueryDevice()
	if err != nil {
		return nil, err
	}
	// ibv_query_gid_table reports every port at once and fails with EINVAL
	// when the buffer is smaller than the number of populated entries.
	capacity := 0
	for p := 1; p <= int(dev.PhysPortCnt); p++ {
		pa, err := c.QueryPort(uint8(p))
		if err != nil {
			return nil, err
		}
		capacity += pa.GIDTableLen
	}
	if capacity == 0 {
		return nil, nil
	}
	buf := make([]C.struct_ibv_gid_entry, capacity)
	n := C.ibv_query_gid_table(c.ctx, &buf[0], C.size_t(capacity), 0)
	if n < 0 {
		return nil, opErrorRC("ibv_query_gid_table", C.int(n))
	}
	var entries []GIDEntry
	for _, e := range buf[:n] {
		if uint8(e.port_num) != port {
			continue
		}
		entries = append(entries, GIDEntry{
			Index: int(e.gid_index),
			Type:  GIDType(e.gid_type),
			GID:   *(*GID)(unsafe.Pointer(&e.gid)),
		})
	}
	return entries, nil
}
