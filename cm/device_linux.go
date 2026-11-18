//go:build linux && !android && (amd64 || arm64 || loong64 || ppc64le || riscv64 || s390x)

package cm

import (
	"errors"
	"sync"
	"syscall"

	"github.com/wzshiming/rdma/internal/ucma"
	"github.com/wzshiming/rdma/verbs"
)

// device is an RDMA device as librdmacm keeps it (struct cma_device): the
// verbs device, and from its first use an open context, a default
// protection domain, the transport, the link layer of each port and the
// depth limits. Nothing of it is released while the process lives.
type device struct {
	dev   *verbs.Device
	guid  uint64
	index int

	ctx       *verbs.Context
	pd        *verbs.PD
	transport verbs.TransportType
	links     []verbs.LinkLayer // of ports 1 and up
	limits    limits
}

var (
	devMu       sync.Mutex // guards the rest and every device's fields until it is opened
	devices     []*device
	initialized bool
)

// listDevices lists the host's devices; a test replaces it.
var listDevices = verbs.Devices

// initialize checks what librdmacm's initialisation checks before the first
// command, the kernel's ABI version and the host's devices, and does nothing
// once that succeeded; a failure is tried again by the next call. A
// libibverbs that cannot be loaded fails op, the operation of the caller, as
// librdmacm's absence did; the rest fails rdma_create_event_channel, which
// runs the checks in librdmacm, and no device at all is ENODEV.
func initialize(op string) error {
	devMu.Lock()
	defer devMu.Unlock()
	if initialized {
		return nil
	}
	if err := ucma.CheckABIVersion(); err != nil {
		return opError("rdma_create_event_channel", err)
	}
	if err := syncDevices(); err != nil {
		var opErr *verbs.OpError
		var errno syscall.Errno
		if errors.Is(err, errors.ErrUnsupported) && errors.As(err, &opErr) && !errors.As(err, &errno) {
			return opError(op, opErr.Err)
		}
		return opError("rdma_create_event_channel", syscall.ENODEV)
	}
	initialized = true
	return nil
}

// syncDevices lists the host's devices again, as sync_devices_list does, and
// keeps the entries of those already known; none is ENODEV. The caller holds
// devMu.
func syncDevices() error {
	list, err := listDevices()
	if err != nil {
		return err
	}
	if len(list) == 0 {
		return syscall.ENODEV
	}
	next := make([]*device, 0, len(list))
	for _, dev := range list {
		d := entryOf(dev)
		if d == nil {
			d = &device{dev: dev, guid: dev.GUID(), index: dev.Index()}
		}
		next = append(next, d)
	}
	devices = next
	return nil
}

// entryOf returns the entry of dev in the table, or nil.
func entryOf(dev *verbs.Device) *device {
	for _, d := range devices {
		if d.dev.Name() == dev.Name() && d.guid == dev.GUID() && d.index == dev.Index() {
			return d
		}
	}
	return nil
}

// findDevice returns the device the kernel names by its node GUID and
// index, or nil. The caller holds devMu.
func findDevice(guid uint64, index int) *device {
	for _, d := range devices {
		if matchDevice(d.guid, d.index, guid, index) {
			return d
		}
	}
	return nil
}

// lookupDevice returns the device the kernel names by its node GUID and
// index, opened, as ucma_get_device does: the table is searched, then
// listed again for a device added since; no such device is ENODEV.
func lookupDevice(guid uint64, index int) (*device, error) {
	devMu.Lock()
	defer devMu.Unlock()
	d := findDevice(guid, index)
	if d == nil {
		if syncDevices() != nil {
			return nil, syscall.ENODEV
		}
		d = findDevice(guid, index)
	}
	if d == nil {
		return nil, syscall.ENODEV
	}
	if err := d.open(); err != nil {
		return nil, err
	}
	return d, nil
}

// open opens the device on its first use as ucma_init_device does, a device
// that does not open being ENODEV, and allocates its default protection
// domain. The caller holds devMu.
func (d *device) open() error {
	if d.ctx == nil {
		ctx, err := d.dev.Open()
		if err != nil {
			return syscall.ENODEV
		}
		attr, err := ctx.QueryDevice()
		if err != nil {
			ctx.Close()
			return cause(err)
		}
		links := make([]verbs.LinkLayer, attr.PhysPortCnt)
		for i := range links {
			if port, err := ctx.QueryPort(uint8(i + 1)); err == nil {
				links[i] = port.LinkLayer
			}
		}
		d.ctx, d.transport, d.links = ctx, ctx.TransportType(), links
		d.limits = limits{initiatorDepth: uint8(attr.MaxQPInitRDAtom), responderResources: uint8(attr.MaxQPRDAtom)}
	}
	if d.pd == nil {
		pd, err := d.ctx.AllocPD()
		if err != nil {
			return cause(err)
		}
		d.pd = pd
	}
	return nil
}

// linkLayer returns the link layer of port, unspecified for a port the
// device does not have.
func (d *device) linkLayer(port uint8) verbs.LinkLayer {
	if port == 0 || int(port) > len(d.links) {
		return verbs.LinkLayerUnspecified
	}
	return d.links[port-1]
}
