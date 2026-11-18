//go:build linux && !android && (amd64 || arm64 || loong64 || ppc64le || riscv64 || s390x)

package cm_test

import (
	"context"
	"errors"
	"net/netip"
	"syscall"
	"testing"

	"github.com/wzshiming/rdma/internal/rdmaabi"
	"github.com/wzshiming/rdma/internal/rdmatest"
	"github.com/wzshiming/rdma/verbs"
)

// resolveOnly resolves remote from the device's IP with im and leaves the
// ID without a queue pair; the test ends by closing it.
func resolveOnly(t *testing.T, im impl, dev rdmatest.Dev, remote netip.AddrPort) conn {
	t.Helper()
	ctx, cancel := context.WithTimeout(context.Background(), timeout)
	defer cancel()
	id, err := im.resolve(ctx, netip.AddrPortFrom(dev.IP, 0), remote)
	if err != nil {
		t.Fatalf("Resolve(%v) error: %v", remote, err)
	}
	t.Cleanup(func() { id.Close() })
	return id
}

// rcAttr describes an RC queue pair for id on a completion queue new on
// ctx; the test ends by closing id, then the queue.
func rcAttr(t *testing.T, id conn, ctx *verbs.Context) *verbs.QPInitAttr {
	t.Helper()
	cq, err := ctx.CreateCQ(16, nil)
	if err != nil {
		t.Fatalf("CreateCQ() error: %v", err)
	}
	t.Cleanup(func() {
		id.Close()
		cq.Close()
	})
	return &verbs.QPInitAttr{SendCQ: cq, RecvCQ: cq, Type: verbs.QPTypeRC, Cap: verbs.QPCap{MaxSendWR: 4, MaxRecvWR: 4, MaxSendSGE: 1, MaxRecvSGE: 1}}
}

// openDevice opens the device name in a context of its own, closed when the
// test ends.
func openDevice(t *testing.T, name string) *verbs.Context {
	t.Helper()
	devs, err := verbs.Devices()
	if err != nil {
		t.Fatalf("Devices() error: %v", err)
	}
	for _, d := range devs {
		if d.Name() == name {
			ctx, err := d.Open()
			if err != nil {
				t.Fatalf("Open(%s) error: %v", name, err)
			}
			t.Cleanup(func() { ctx.Close() })
			return ctx
		}
	}
	t.Fatalf("no device %s", name)
	return nil
}

// TestCreateQPTakesTheDefaultPD creates the queue pairs of two IDs on one
// device without a protection domain: both take the device's default one,
// and both are in INIT on the ID's port, as the kernel's attributes have it;
// siw's query fills in no port, which those attributes set on iWARP too, so
// on iWARP the state alone is checked.
func TestCreateQPTakesTheDefaultPD(t *testing.T) {
	rdmatest.Device(t)
	forEachImpl(t, func(t *testing.T, im impl) {
		dev, ln := listen(t, im)
		var pds [2]uint64
		for i := range pds {
			id := resolveOnly(t, im, dev, ln.Addr())
			qp, err := id.CreateQP(nil, rcAttr(t, id, id.Device()))
			if err != nil {
				t.Fatalf("CreateQP(nil) error: %v", err)
			}
			a, err := qp.Query(verbs.QPAttrState | verbs.QPAttrPort)
			if err != nil {
				t.Fatalf("QP.Query() error: %v", err)
			}
			if a.State != verbs.QPStateInit || !onIWARP(id) && a.Port != id.Port() {
				t.Errorf("queue pair in %v on port %d, want %v on port %d", a.State, a.Port, verbs.QPStateInit, id.Port())
			}
			pds[i] = (*rdmaabi.IbvQp)(qp.Pointer()).Pd
		}
		if pds[0] == 0 || pds[0] != pds[1] {
			t.Errorf("the queue pairs of two IDs on one device have the protection domains %#x and %#x, want the device's default for both", pds[0], pds[1])
		}
	})
}

// TestCreateQPRefusesAPDOfAnotherContext creates a queue pair with the
// protection domain of another context of the same device: EINVAL, and no
// queue pair is left behind.
func TestCreateQPRefusesAPDOfAnotherContext(t *testing.T) {
	rdmatest.Device(t)
	forEachImpl(t, func(t *testing.T, im impl) {
		dev, ln := listen(t, im)
		pd, err := openDevice(t, dev.Name).AllocPD()
		if err != nil {
			t.Fatalf("AllocPD() error: %v", err)
		}
		t.Cleanup(func() { pd.Close() })
		id := resolveOnly(t, im, dev, ln.Addr())
		attr := rcAttr(t, id, id.Device())
		qp, err := id.CreateQP(pd, attr)
		var opErr *verbs.OpError
		if qp != nil || !errors.As(err, &opErr) || opErr.Op != "rdma_create_qp" || !errors.Is(err, syscall.EINVAL) {
			t.Fatalf("CreateQP(a PD of another context) = %v, %v; want a *verbs.OpError of rdma_create_qp with EINVAL", qp, err)
		}
		if id.QP() != nil {
			t.Errorf("QP() after the refused CreateQP = %v, want nil", id.QP())
		}
		if _, err := id.CreateQP(nil, attr); err != nil {
			t.Errorf("CreateQP(nil) after the refused one: %v", err)
		}
	})
}

// TestDeviceCloseReleasesNothing closes the device of an ID: the device
// stays open for the ID's queue pair and for the next ID.
func TestDeviceCloseReleasesNothing(t *testing.T) {
	rdmatest.Device(t)
	forEachImpl(t, func(t *testing.T, im impl) {
		dev, ln := listen(t, im)
		first := resolveOnly(t, im, dev, ln.Addr())
		if err := first.Device().Close(); err != nil {
			t.Fatalf("Device().Close() error: %v", err)
		}
		second := resolve(t, im, dev, ln.Addr())
		if _, err := first.CreateQP(nil, rcAttr(t, first, second.Device())); err != nil {
			t.Errorf("CreateQP() after Device().Close(): %v", err)
		}
	})
}
