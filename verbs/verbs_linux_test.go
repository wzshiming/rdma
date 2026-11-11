//go:build linux && !android && (amd64 || arm64 || loong64 || ppc64le || riscv64 || s390x)

package verbs_test

import (
	"bytes"
	"errors"
	"net/netip"
	"os"
	"syscall"
	"testing"
	"unsafe"

	"github.com/wzshiming/rdma/internal/rdmatest"
	"github.com/wzshiming/rdma/verbs"
)

func TestOpErrorErrno(t *testing.T) {
	err := error(&verbs.OpError{Op: "ibv_alloc_pd", Err: syscall.ENOMEM})
	if want := "verbs: ibv_alloc_pd: cannot allocate memory"; err.Error() != want {
		t.Errorf("Error() = %q, want %q", err.Error(), want)
	}
	if !errors.Is(err, syscall.ENOMEM) {
		t.Error("errors.Is(err, syscall.ENOMEM) = false, want true")
	}
}

// Devices lists whatever the host has, possibly nothing; only a kernel
// without the RDMA stack makes ibv_get_device_list fail, with ENOSYS.
func TestDevicesOnAnyHost(t *testing.T) {
	devs, err := verbs.Devices()
	if err != nil && !errors.Is(err, syscall.ENOSYS) {
		t.Fatalf("Devices() error: %v", err)
	}
	for _, d := range devs {
		if d.Name() == "" {
			t.Errorf("Devices() has a device without a name: %+v", d)
		}
	}
}

// openDevice opens the rdmatest device by name and closes it with the test.
func openDevice(t *testing.T) (rdmatest.Dev, *verbs.Context) {
	t.Helper()
	dev := rdmatest.Device(t)
	devs, err := verbs.Devices()
	if err != nil {
		t.Fatalf("Devices() error: %v", err)
	}
	for _, d := range devs {
		if d.Name() != dev.Name {
			continue
		}
		ctx, err := d.Open()
		if err != nil {
			t.Fatalf("Open(%s) error: %v", dev.Name, err)
		}
		t.Cleanup(func() {
			if err := ctx.Close(); err != nil {
				t.Errorf("Close() error: %v", err)
			}
		})
		return dev, ctx
	}
	t.Fatalf("Devices() = %d devices, none named %q", len(devs), dev.Name)
	return dev, nil
}

func TestDevice(t *testing.T) {
	dev, ctx := openDevice(t)
	port := uint8(dev.Port)

	attr, err := ctx.QueryDevice()
	if err != nil {
		t.Fatalf("QueryDevice() error: %v", err)
	}
	if int(attr.PhysPortCnt) < dev.Port || attr.MaxQP <= 0 || attr.MaxQPWR <= 0 || attr.MaxSGE <= 0 || attr.MaxCQE <= 0 || attr.MaxMRSize == 0 {
		t.Errorf("QueryDevice() = %+v, want positive limits and at least %d ports", attr, dev.Port)
	}
	devs, _ := verbs.Devices()
	for _, d := range devs {
		if d.Name() == dev.Name && d.GUID() != attr.NodeGUID {
			t.Errorf("Device.GUID() = %#x, QueryDevice().NodeGUID = %#x", d.GUID(), attr.NodeGUID)
		}
	}

	pa, err := ctx.QueryPort(port)
	if err != nil {
		t.Fatalf("QueryPort(%d) error: %v", port, err)
	}
	if pa.State != verbs.PortActive {
		t.Errorf("QueryPort(%d).State = %v, want %v", port, pa.State, verbs.PortActive)
	}
	if pa.ActiveMTU.Bytes() < 256 || pa.MaxMTU < pa.ActiveMTU {
		t.Errorf("QueryPort(%d) MTU: active %v, max %v", port, pa.ActiveMTU, pa.MaxMTU)
	}
	if pa.GIDTableLen <= dev.GIDIndex {
		t.Errorf("QueryPort(%d).GIDTableLen = %d, want > %d", port, pa.GIDTableLen, dev.GIDIndex)
	}
	if _, err := ctx.QueryPort(0); err == nil {
		t.Error("QueryPort(0) succeeded, want an error")
	}

	gids, err := ctx.GIDTable(port)
	if err != nil {
		t.Fatalf("GIDTable(%d) error: %v", port, err)
	}
	found := false
	for _, g := range gids {
		if g.Index != dev.GIDIndex {
			continue
		}
		found = true
		if g.GID.IsZero() {
			t.Errorf("GIDTable(%d)[%d].GID is zero", port, g.Index)
		}
		if pa.LinkLayer == verbs.LinkLayerEthernet {
			if g.Type != verbs.GIDTypeRoCEv2 {
				t.Errorf("GIDTable(%d)[%d].Type = %v, want %v", port, g.Index, g.Type, verbs.GIDTypeRoCEv2)
			}
			if ip := netip.AddrFrom16(g.GID).Unmap(); ip != dev.IP {
				t.Errorf("GIDTable(%d)[%d].GID = %v, want %v", port, g.Index, g.GID, dev.IP)
			}
		}
	}
	if !found {
		t.Errorf("GIDTable(%d) = %v, no entry with index %d", port, gids, dev.GIDIndex)
	}

	if ctx.Pointer() == nil {
		t.Fatal("Pointer() = nil")
	}
	alias := verbs.ContextFromPointer(ctx.Pointer())
	if _, err := alias.QueryDevice(); err != nil {
		t.Errorf("ContextFromPointer(...).QueryDevice() error: %v", err)
	}
	if err := alias.Close(); err != nil {
		t.Errorf("ContextFromPointer(...).Close() error: %v", err)
	}
	if _, err := ctx.QueryDevice(); err != nil {
		t.Errorf("QueryDevice() after closing the alias error: %v", err)
	}
}

// RoCE devices such as rxe report the InfiniBand transport; rdmatest marks
// an iWARP device (siw) with GID index -1.
func TestDeviceReportsItsTransportType(t *testing.T) {
	dev, ctx := openDevice(t)
	want := verbs.TransportIB
	if dev.GIDIndex < 0 {
		want = verbs.TransportIWARP
	}
	devs, err := verbs.Devices()
	if err != nil {
		t.Fatalf("Devices() error: %v", err)
	}
	for _, d := range devs {
		if d.Name() != dev.Name {
			continue
		}
		if got := d.TransportType(); got != want {
			t.Errorf("Device(%s).TransportType() = %v, want %v", dev.Name, got, want)
		}
	}
	got := ctx.TransportType()
	if got != want {
		t.Errorf("Context.TransportType() = %v, want %v", got, want)
	}
	// The context of an rdma_cm id is wrapped this way.
	if got := verbs.ContextFromPointer(ctx.Pointer()).TransportType(); got != want {
		t.Errorf("ContextFromPointer(...).TransportType() = %v, want %v", got, want)
	}
	t.Logf("%s reports transport %v (%d)", dev.Name, got, got)
}

func allocPD(t *testing.T, ctx *verbs.Context) *verbs.PD {
	t.Helper()
	pd, err := ctx.AllocPD()
	if err != nil {
		t.Fatalf("AllocPD() error: %v", err)
	}
	t.Cleanup(func() {
		if err := pd.Close(); err != nil {
			t.Errorf("PD.Close() error: %v", err)
		}
	})
	return pd
}

func TestRegisterMemory(t *testing.T) {
	_, ctx := openDevice(t)
	pd := allocPD(t, ctx)

	buf := bytes.Repeat([]byte("go"), 2048)
	mr, err := pd.RegisterMemory(buf, verbs.AccessLocalWrite|verbs.AccessRemoteWrite|verbs.AccessRemoteRead)
	if err != nil {
		t.Fatalf("RegisterMemory() error: %v", err)
	}
	if got := mr.Bytes(); unsafe.SliceData(got) != unsafe.SliceData(buf) || len(got) != len(buf) {
		t.Errorf("Bytes() does not alias the registered slice")
	}
	if mr.LKey() == 0 || mr.RKey() == 0 {
		t.Errorf("LKey() = %d, RKey() = %d, want non-zero keys", mr.LKey(), mr.RKey())
	}
	sub := buf[1024:1536]
	sge := mr.SGE(sub)
	if sge.Addr != uint64(uintptr(unsafe.Pointer(unsafe.SliceData(sub)))) || sge.Length != 512 || sge.LKey != mr.LKey() {
		t.Errorf("SGE() = %+v, want addr %#x, length 512, lkey %d", sge, uintptr(unsafe.Pointer(unsafe.SliceData(sub))), mr.LKey())
	}
	t.Run("SGEOutsideRegionPanics", func(t *testing.T) {
		defer func() {
			if recover() == nil {
				t.Error("SGE() of a slice outside the region did not panic")
			}
		}()
		mr.SGE(make([]byte, 8))
	})
	if mr.Pointer() == nil {
		t.Error("Pointer() = nil")
	}
	if err := mr.Close(); err != nil {
		t.Fatalf("Close() error: %v", err)
	}
	if err := mr.Close(); err != nil {
		t.Errorf("second Close() error: %v", err)
	}
	if _, err := pd.RegisterMemory(nil, verbs.AccessLocalWrite); !errors.Is(err, syscall.EINVAL) {
		t.Errorf("RegisterMemory(nil) error = %v, want EINVAL", err)
	}
}

func TestAllocMemory(t *testing.T) {
	_, ctx := openDevice(t)
	pd := allocPD(t, ctx)

	mr, err := pd.AllocMemory(3*os.Getpagesize()/2, verbs.AccessLocalWrite|verbs.AccessRemoteWrite)
	if err != nil {
		t.Fatalf("AllocMemory() error: %v", err)
	}
	b := mr.Bytes()
	if len(b) != 3*os.Getpagesize()/2 {
		t.Errorf("len(Bytes()) = %d, want %d", len(b), 3*os.Getpagesize()/2)
	}
	if addr := uintptr(unsafe.Pointer(unsafe.SliceData(b))); addr%uintptr(os.Getpagesize()) != 0 {
		t.Errorf("Bytes() starts at %#x, want page alignment", addr)
	}
	for i := range b {
		b[i] = byte(i)
	}
	if mr.SGE(b[:16]).Length != 16 || mr.LKey() == 0 {
		t.Errorf("SGE/LKey of allocated memory: %+v, %d", mr.SGE(b[:16]), mr.LKey())
	}
	if err := mr.Close(); err != nil {
		t.Fatalf("Close() error: %v", err)
	}
	if _, err := pd.AllocMemory(0, verbs.AccessLocalWrite); !errors.Is(err, syscall.EINVAL) {
		t.Errorf("AllocMemory(0) error = %v, want EINVAL", err)
	}
}
