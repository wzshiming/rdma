//go:build linux && !android && (amd64 || arm64 || loong64 || ppc64le || riscv64 || s390x)

package verbs

import (
	"runtime"
	"unsafe"

	"github.com/wzshiming/rdma/internal/dynlib"
	"github.com/wzshiming/rdma/internal/rdmaabi"
)

// deviceList keeps the array of ibv_get_device_list alive while any Device
// from it is reachable; opened devices stay valid on their own.
type deviceList struct {
	list uintptr // struct ibv_device **
}

// Device is an RDMA device returned by Devices.
type Device struct {
	dev  uintptr // struct ibv_device *, opaque to the package
	list *deviceList
	name string
	guid uint64
}

// Devices returns the RDMA devices of the host.
func Devices() ([]*Device, error) {
	f, err := load("ibv_get_device_list")
	if err != nil {
		return nil, err
	}
	var n int32
	list, errno := f.getDeviceList.Ptr(uintptr(unsafe.Pointer(&n)), 0, 0, 0, 0, 0)
	if list == 0 {
		return nil, opError("ibv_get_device_list", errno)
	}
	dl := &deviceList{list: list}
	runtime.AddCleanup(dl, func(l uintptr) { f.freeDeviceList.Int(l, 0, 0, 0, 0, 0) }, list)
	devs := make([]*Device, 0, int(n))
	for _, d := range unsafe.Slice(dynlib.Pointer[uintptr](list), int(n)) {
		name, _ := f.getDeviceName.Ptr(d, 0, 0, 0, 0, 0)
		// The __be64 GUID comes back in the return register.
		g, _ := f.getDeviceGUID.Ptr(d, 0, 0, 0, 0, 0)
		devs = append(devs, &Device{dev: d, list: dl, name: goString(name), guid: be64(uint64(g))})
	}
	return devs, nil
}

// Name returns the kernel name of the device, such as "rxe0" or "mlx5_0".
func (d *Device) Name() string { return d.name }

// GUID returns the node GUID of the device.
func (d *Device) GUID() uint64 { return d.guid }

// Open opens the device.
func (d *Device) Open() (*Context, error) {
	f, err := load("ibv_open_device")
	if err != nil {
		return nil, err
	}
	ctx, errno := f.openDevice.Ptr(d.dev, 0, 0, 0, 0, 0)
	runtime.KeepAlive(d)
	if ctx == 0 {
		return nil, opError("ibv_open_device", errno)
	}
	return &Context{ctx: dynlib.Pointer[rdmaabi.IbvContext](ctx), owned: true}, nil
}

// ForkInit wraps ibv_fork_init, which makes registered memory safe across
// fork. Kernels 5.12 and newer do not need it; the package never calls it.
func ForkInit() error {
	f, err := load("ibv_fork_init")
	if err != nil {
		return err
	}
	if rc, _ := f.forkInit.Int(0, 0, 0, 0, 0, 0); rc != 0 {
		return opErrorRC("ibv_fork_init", rc)
	}
	return nil
}

// Context is an open device (struct ibv_context).
type Context struct {
	ctx   *rdmaabi.IbvContext
	owned bool
	async fdWaiter
}

// ContextFromPointer wraps a struct ibv_context pointer owned by someone else,
// such as the verbs context of an rdma_cm id. Close does not close the device.
func ContextFromPointer(p unsafe.Pointer) *Context {
	return &Context{ctx: (*rdmaabi.IbvContext)(p)}
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
	f, err := load("ibv_close_device")
	if err != nil {
		return err
	}
	if rc, errno := f.closeDevice.Int(caddr(ctx), 0, 0, 0, 0, 0); rc != 0 {
		return opError("ibv_close_device", errno)
	}
	return nil
}

// QueryDevice returns the attributes and limits of the device.
func (c *Context) QueryDevice() (*DeviceAttr, error) {
	f, err := load("ibv_query_device")
	if err != nil {
		return nil, err
	}
	var a rdmaabi.IbvDeviceAttr
	if rc, _ := f.queryDevice.Int(caddr(c.ctx), uintptr(unsafe.Pointer(&a)), 0, 0, 0, 0); rc != 0 {
		return nil, opErrorRC("ibv_query_device", rc)
	}
	return &DeviceAttr{
		FWVer:           cString(a.Fw_ver[:]),
		NodeGUID:        guid(a.Node_guid),
		SysImageGUID:    guid(a.Sys_image_guid),
		MaxMRSize:       a.Max_mr_size,
		PageSizeCap:     a.Page_size_cap,
		VendorID:        a.Vendor_id,
		VendorPartID:    a.Vendor_part_id,
		HWVer:           a.Hw_ver,
		MaxQP:           int(a.Max_qp),
		MaxQPWR:         int(a.Max_qp_wr),
		DeviceCapFlags:  a.Device_cap_flags,
		MaxSGE:          int(a.Max_sge),
		MaxSGERD:        int(a.Max_sge_rd),
		MaxCQ:           int(a.Max_cq),
		MaxCQE:          int(a.Max_cqe),
		MaxMR:           int(a.Max_mr),
		MaxPD:           int(a.Max_pd),
		MaxQPRDAtom:     int(a.Max_qp_rd_atom),
		MaxResRDAtom:    int(a.Max_res_rd_atom),
		MaxQPInitRDAtom: int(a.Max_qp_init_rd_atom),
		MaxMW:           int(a.Max_mw),
		MaxAH:           int(a.Max_ah),
		MaxSRQ:          int(a.Max_srq),
		MaxSRQWR:        int(a.Max_srq_wr),
		MaxSRQSGE:       int(a.Max_srq_sge),
		MaxPkeys:        a.Max_pkeys,
		LocalCAAckDelay: a.Local_ca_ack_delay,
		PhysPortCnt:     a.Phys_port_cnt,
	}, nil
}

// QueryPort returns the attributes of a port; ports are numbered from 1.
func (c *Context) QueryPort(port uint8) (*PortAttr, error) {
	f, err := load("ibv_query_port")
	if err != nil {
		return nil, err
	}
	// The exported ibv_query_port is declared on the compat prefix of the
	// struct, but a provider may fill a whole struct ibv_port_attr.
	var a rdmaabi.IbvPortAttr
	if rc, _ := f.queryPort.Int(caddr(c.ctx), uintptr(port), uintptr(unsafe.Pointer(&a)), 0, 0, 0); rc != 0 {
		return nil, opErrorRC("ibv_query_port", rc)
	}
	return &PortAttr{
		State:         PortState(a.State),
		MaxMTU:        MTU(a.Max_mtu),
		ActiveMTU:     MTU(a.Active_mtu),
		GIDTableLen:   int(a.Gid_tbl_len),
		PortCapFlags:  a.Port_cap_flags,
		MaxMsgSize:    a.Max_msg_sz,
		BadPkeyCntr:   a.Bad_pkey_cntr,
		QKeyViolCntr:  a.Qkey_viol_cntr,
		PkeyTableLen:  a.Pkey_tbl_len,
		LID:           a.Lid,
		SMLID:         a.Sm_lid,
		LMC:           a.Lmc,
		MaxVLNum:      a.Max_vl_num,
		SMSL:          a.Sm_sl,
		SubnetTimeout: a.Subnet_timeout,
		InitTypeReply: a.Init_type_reply,
		ActiveWidth:   a.Active_width,
		ActiveSpeed:   a.Active_speed,
		PhysState:     a.Phys_state,
		LinkLayer:     LinkLayer(a.Link_layer),
	}, nil
}

// GIDTable returns the populated GID table entries of a port with their index
// and type (IB, RoCE v1 or RoCE v2).
func (c *Context) GIDTable(port uint8) ([]GIDEntry, error) {
	f, err := load("ibv_query_gid_table")
	if err != nil {
		return nil, err
	}
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
	buf := make([]rdmaabi.IbvGidEntry, capacity)
	n, _ := f.queryGIDTable.SSize(caddr(c.ctx), uintptr(unsafe.Pointer(&buf[0])), uintptr(capacity), 0, unsafe.Sizeof(buf[0]), 0)
	if n < 0 {
		return nil, opErrorRC("ibv_query_gid_table", int32(n))
	}
	var entries []GIDEntry
	for i := range buf[:n] {
		if uint8(buf[i].Port_num) != port {
			continue
		}
		entries = append(entries, gidEntryFromC(&buf[i]))
	}
	return entries, nil
}

func gidEntryFromC(e *rdmaabi.IbvGidEntry) GIDEntry {
	return GIDEntry{Index: int(e.Gid_index), Type: GIDType(e.Gid_type), GID: GID(e.Gid)}
}
