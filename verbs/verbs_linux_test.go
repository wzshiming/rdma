//go:build linux && cgo

package verbs_test

import (
	"errors"
	"net/netip"
	"syscall"
	"testing"

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
