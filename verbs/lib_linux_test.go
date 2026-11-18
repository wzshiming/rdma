//go:build linux && !android && (amd64 || arm64 || loong64 || ppc64le || riscv64 || s390x)

package verbs

import (
	"errors"
	"os"
	"strings"
	"syscall"
	"testing"
	"unsafe"

	"github.com/wzshiming/rdma/internal/dynlib"
	"github.com/wzshiming/rdma/internal/rdmaabi"
	"github.com/wzshiming/rdma/internal/rdmatest"
)

// useLibrary makes the next use of the package open the library at name,
// whatever it has loaded already, until the test ends.
func useLibrary(t *testing.T, name string) {
	t.Helper()
	libMu.Lock()
	defer libMu.Unlock()
	saved, savedName := lib.Swap(nil), libName
	libName = name
	t.Cleanup(func() {
		libMu.Lock()
		defer libMu.Unlock()
		libName = savedName
		lib.Store(saved)
	})
}

const missingLibrary = "/nonexistent/libibverbs.so.1"

func TestEntryPointsWithoutLibrary(t *testing.T) {
	useLibrary(t, missingLibrary)
	tests := []struct {
		name string
		call func() error
	}{
		{"Devices", func() error { _, err := Devices(); return err }},
		{"ForkInit", ForkInit},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			err := tt.call()
			var opErr *OpError
			if !errors.As(err, &opErr) {
				t.Fatalf("err = %T (%v), want *OpError", err, err)
			}
			if !errors.Is(err, errors.ErrUnsupported) {
				t.Errorf("%v does not match errors.ErrUnsupported", err)
			}
			if errors.Is(err, syscall.ENOSYS) {
				t.Errorf("%v matches ENOSYS, which means a host without devices", err)
			}
			if !strings.Contains(err.Error(), "libibverbs.so.1") {
				t.Errorf("%q does not name libibverbs.so.1", err)
			}
		})
	}
}

func TestLibraryLoadsAfterAFailedAttempt(t *testing.T) {
	useLibrary(t, missingLibrary)
	if _, err := Devices(); !errors.Is(err, errors.ErrUnsupported) {
		t.Fatalf("Devices() with %s: %v, want errors.ErrUnsupported", missingLibrary, err)
	}
	libMu.Lock()
	libName = "libibverbs.so.1"
	libMu.Unlock()
	// Once loaded, the library answers: with devices, or with its errno
	// (ENOSYS on a kernel without the RDMA stack).
	if _, err := Devices(); err != nil {
		var errno syscall.Errno
		if !errors.As(err, &errno) {
			t.Fatalf("Devices() once the library is loadable: %v, want nil or an errno", err)
		}
	}
}

// ibv_get_device_index is optional: a library without it loads, and its
// devices report an unknown index.
func TestLibraryWithoutDeviceIndexLoads(t *testing.T) {
	useLibrary(t, "libibverbs.so.1")
	libMu.Lock()
	name := deviceIndexName
	deviceIndexName = "ibv_get_device_index_of_a_newer_library"
	libMu.Unlock()
	t.Cleanup(func() {
		libMu.Lock()
		defer libMu.Unlock()
		deviceIndexName = name
	})
	devs, err := Devices()
	if err != nil {
		var errno syscall.Errno
		if !errors.As(err, &errno) {
			t.Fatalf("Devices() from a library without %s: %v, want nil or an errno", name, err)
		}
	}
	for _, d := range devs {
		if i := d.Index(); i != -1 {
			t.Errorf("Device(%s).Index() from a library without %s = %d, want -1", d.Name(), name, i)
		}
	}
	t.Logf("%d devices report -1", len(devs))
}

// ibv_query_ece and ibv_set_ece are optional: a library without them, older
// than rdma-core 31, loads, and its queue pairs report EOPNOTSUPP, as those
// of a provider without ECE do, without a call into the library.
func TestLibraryWithoutECELoads(t *testing.T) {
	useLibrary(t, "libibverbs.so.1")
	libMu.Lock()
	query, set := queryECEName, setECEName
	queryECEName, setECEName = "ibv_query_ece_of_a_newer_library", "ibv_set_ece_of_a_newer_library"
	libMu.Unlock()
	t.Cleanup(func() {
		libMu.Lock()
		defer libMu.Unlock()
		queryECEName, setECEName = query, set
	})
	if _, err := Devices(); err != nil {
		var errno syscall.Errno
		if !errors.As(err, &errno) {
			t.Fatalf("Devices() from a library without %s and %s: %v, want nil or an errno", query, set, err)
		}
	}
	qp := fakeQP(t, QPTypeRC)
	ece, err := qp.QueryECE()
	var opErr *OpError
	if ece != (ECE{}) || !errors.As(err, &opErr) || opErr.Op != "ibv_query_ece" || !errors.Is(err, syscall.EOPNOTSUPP) {
		t.Errorf("QueryECE() = %+v, %v; want none and an *OpError of ibv_query_ece with EOPNOTSUPP", ece, err)
	}
	if err := qp.SetECE(ECE{VendorID: 0x02c9, Options: 1}); !errors.As(err, &opErr) || opErr.Op != "ibv_set_ece" || !errors.Is(err, syscall.EOPNOTSUPP) {
		t.Errorf("SetECE() error = %v, want an *OpError of ibv_set_ece with EOPNOTSUPP", err)
	}
}

func TestCreateAHWithoutLibrary(t *testing.T) {
	useLibrary(t, missingLibrary)
	_, err := PDFromPointer(nil).CreateAH(&AHAttr{})
	var opErr *OpError
	if !errors.As(err, &opErr) || opErr.Op != "ibv_create_ah" {
		t.Fatalf("CreateAH() error = %v, want an *OpError of ibv_create_ah", err)
	}
	if !errors.Is(err, errors.ErrUnsupported) || !strings.Contains(err.Error(), "libibverbs.so.1") {
		t.Errorf("CreateAH() error = %v, want errors.ErrUnsupported naming libibverbs.so.1", err)
	}
}

func TestCreateAHFromWCWithoutLibrary(t *testing.T) {
	useLibrary(t, missingLibrary)
	_, err := PDFromPointer(nil).CreateAHFromWC(&WC{Flags: WCGRH}, make([]byte, 40), 1)
	var opErr *OpError
	if !errors.As(err, &opErr) || opErr.Op != "ibv_create_ah_from_wc" || !errors.Is(err, errors.ErrUnsupported) {
		t.Fatalf("CreateAHFromWC() error = %v, want an *OpError of ibv_create_ah_from_wc matching errors.ErrUnsupported", err)
	}
}

// libibverbs reads the whole global routing header, so a shorter buffer is
// refused before any call.
func TestCreateAHFromWCWithAShortHeader(t *testing.T) {
	useLibrary(t, missingLibrary)
	_, err := PDFromPointer(nil).CreateAHFromWC(&WC{Flags: WCGRH}, make([]byte, 39), 1)
	var opErr *OpError
	if !errors.As(err, &opErr) || opErr.Op != "ibv_create_ah_from_wc" || !errors.Is(err, syscall.EINVAL) {
		t.Fatalf("CreateAHFromWC() with 39 bytes of header error = %v, want an *OpError of ibv_create_ah_from_wc matching EINVAL", err)
	}
}

func TestMulticastWithoutLibrary(t *testing.T) {
	useLibrary(t, missingLibrary)
	qp := QPFromPointer(nil)
	for op, call := range map[string]func(GID, uint16) error{
		"ibv_attach_mcast": qp.AttachMulticast,
		"ibv_detach_mcast": qp.DetachMulticast,
	} {
		err := call(GID{0: 0xff}, 0)
		var opErr *OpError
		if !errors.As(err, &opErr) || opErr.Op != op || !errors.Is(err, errors.ErrUnsupported) {
			t.Errorf("%s: error = %v, want an *OpError of %s matching errors.ErrUnsupported", op, err, op)
		}
	}
}

// A wrapper of an address handle made elsewhere destroys nothing: its Close
// does not even need the library.
func TestAHFromPointerCloseDestroysNothing(t *testing.T) {
	useLibrary(t, missingLibrary)
	ah := AHFromPointer(cMemory(t, 64))
	if err := ah.Close(); err != nil {
		t.Errorf("Close() error = %v, want nil", err)
	}
	if ah.Pointer() != nil {
		t.Errorf("Pointer() after Close() = %p, want nil", ah.Pointer())
	}
	if err := ah.Close(); err != nil {
		t.Errorf("second Close() error = %v, want nil", err)
	}
}

func TestCreateSRQWithoutLibrary(t *testing.T) {
	useLibrary(t, missingLibrary)
	_, err := PDFromPointer(nil).CreateSRQ(&SRQInitAttr{Attr: SRQAttr{MaxWR: 1, MaxSGE: 1}})
	var opErr *OpError
	if !errors.As(err, &opErr) || opErr.Op != "ibv_create_srq" {
		t.Fatalf("CreateSRQ() error = %v, want an *OpError of ibv_create_srq", err)
	}
	if !errors.Is(err, errors.ErrUnsupported) || !strings.Contains(err.Error(), "libibverbs.so.1") {
		t.Errorf("CreateSRQ() error = %v, want errors.ErrUnsupported naming libibverbs.so.1", err)
	}
}

// A wrapper of a shared receive queue made elsewhere destroys nothing: its
// Close does not even need the library.
func TestSRQFromPointerCloseDestroysNothing(t *testing.T) {
	useLibrary(t, missingLibrary)
	srq := fakeSRQ(t)
	if err := srq.Close(); err != nil {
		t.Errorf("Close() error = %v, want nil", err)
	}
	if srq.Pointer() != nil {
		t.Errorf("Pointer() after Close() = %p, want nil", srq.Pointer())
	}
}

// fakeVerbsContext returns a device context in C memory that is the context
// member of a struct verbs_context whose sz reaches back 1024 bytes from it,
// past every member the package uses, which are NULL, and the mirror of the
// members before it.
func fakeVerbsContext(t *testing.T) (*rdmaabi.IbvContext, *rdmaabi.VerbsContext) {
	t.Helper()
	ctx := uintptr(cMemory(t, os.Getpagesize())) + 1024
	*dynlib.Pointer[uint64](ctx + abiCompat) = rdmaabi.VerbsAbiIsExtended
	vctx := dynlib.Pointer[rdmaabi.VerbsContext](ctx - unsafe.Sizeof(rdmaabi.VerbsContext{}))
	vctx.Sz = uint64(1024 + abiCompat + 8)
	return dynlib.Pointer[rdmaabi.IbvContext](ctx), vctx
}

// verbsOp finds an extended op as verbs_get_ctx_op does: only in a context
// that is part of a struct verbs_context, only when the library's struct is
// large enough to hold the member, and only when the member is not NULL.
func TestVerbsOpLookup(t *testing.T) {
	openXRCD := unsafe.Offsetof(rdmaabi.VerbsContext{}.Open_xrcd)
	createQPEx := unsafe.Offsetof(rdmaabi.VerbsContext{}.Create_qp_ex)
	ctx, vctx := fakeVerbsContext(t)
	addr := caddr(ctx)
	vctx.Open_xrcd, vctx.Create_qp_ex = 0x1000, 0x2000
	if got := verbsOp(addr, openXRCD).Addr(); got != 0x1000 {
		t.Errorf("open_xrcd = %#x, want %#x", got, 0x1000)
	}
	if got := verbsOp(addr, createQPEx).Addr(); got != 0x2000 {
		t.Errorf("create_qp_ex = %#x, want %#x", got, 0x2000)
	}

	// The struct of a library that has open_xrcd and not yet create_qp_ex,
	// which a later release added in front of it.
	vctx.Sz = uint64(unsafe.Sizeof(*vctx) - openXRCD + abiCompat + 8)
	if got := verbsOp(addr, openXRCD).Addr(); got != 0x1000 {
		t.Errorf("open_xrcd with sz %d = %#x, want %#x", vctx.Sz, got, 0x1000)
	}
	if got := verbsOp(addr, createQPEx).Addr(); got != 0 {
		t.Errorf("create_qp_ex with sz %d = %#x, want none: the struct ends after it", vctx.Sz, got)
	}
	vctx.Sz--
	if got := verbsOp(addr, openXRCD).Addr(); got != 0 {
		t.Errorf("open_xrcd with sz %d = %#x, want none: the struct ends after it", vctx.Sz, got)
	}

	vctx.Sz = uint64(1024 + abiCompat + 8)
	vctx.Open_xrcd = 0
	if got := verbsOp(addr, openXRCD).Addr(); got != 0 {
		t.Errorf("NULL open_xrcd = %#x, want none", got)
	}

	*dynlib.Pointer[uint64](addr + abiCompat) = 0
	if got := verbsOp(addr, createQPEx).Addr(); got != 0 {
		t.Errorf("create_qp_ex of a context that is not a verbs_context = %#x, want none", got)
	}
}

// verbsOp finds create_qp_ex in the context of a device, which the library
// made, and a queue pair comes out of it. The XRC verbs cannot show it on rxe
// or siw: there a failed lookup reads as the EOPNOTSUPP of a found stub.
func TestVerbsOpFindsCreateQPExOfADevice(t *testing.T) {
	dev := rdmatest.Device(t)
	devs, err := Devices()
	if err != nil {
		t.Fatalf("Devices() error: %v", err)
	}
	var ctx *Context
	for _, d := range devs {
		if d.Name() != dev.Name {
			continue
		}
		if ctx, err = d.Open(); err != nil {
			t.Fatalf("Open(%s) error: %v", dev.Name, err)
		}
		t.Cleanup(func() { ctx.Close() })
		break
	}
	if ctx == nil {
		t.Fatalf("Devices() has no device named %q", dev.Name)
	}
	pd, err := ctx.AllocPD()
	if err != nil {
		t.Fatalf("AllocPD() error: %v", err)
	}
	t.Cleanup(func() { pd.Close() })
	cq, err := ctx.CreateCQ(4, nil)
	if err != nil {
		t.Fatalf("CreateCQ() error: %v", err)
	}
	t.Cleanup(func() { cq.Close() })

	create := verbsOp(caddr(ctx.ctx), unsafe.Offsetof(rdmaabi.VerbsContext{}.Create_qp_ex))
	if create.Addr() == 0 {
		t.Fatalf("verbsOp() finds no create_qp_ex in the context of %s", dev.Name)
	}
	ia := rdmaabi.IbvQpInitAttrEx{
		Send_cq:   uint64(caddr(cq.cq)),
		Recv_cq:   uint64(caddr(cq.cq)),
		Cap:       rdmaabi.IbvQpCap{Max_send_wr: 1, Max_recv_wr: 1, Max_send_sge: 1, Max_recv_sge: 1},
		Qp_type:   uint32(QPTypeRC),
		Comp_mask: 1 << 0, // IBV_QP_INIT_ATTR_PD
		Pd:        uint64(uintptr(pd.pd)),
	}
	qp, errno := create.Ptr(caddr(ctx.ctx), uintptr(unsafe.Pointer(&ia)), 0, 0, 0, 0)
	if qp == 0 {
		// libibverbs's stub stands in for a provider without it, as siw's.
		if errno == syscall.EOPNOTSUPP && !strings.HasPrefix(dev.Name, "rxe") && !strings.HasPrefix(dev.Name, "mlx5") {
			t.Skipf("the provider of %s has no create_qp_ex: %v", dev.Name, errno)
		}
		t.Fatalf("create_qp_ex at %#x in the context of %s: %v", create.Addr(), dev.Name, errno)
	}
	q := newQP(dynlib.Pointer[rdmaabi.IbvQp](qp), true)
	t.Cleanup(func() { q.Close() })
	t.Logf("create_qp_ex at %#x in the context of %s created queue pair %#x", create.Addr(), dev.Name, q.Num())
	if q.Type() != QPTypeRC || q.qp.Context != uint64(caddr(ctx.ctx)) {
		t.Errorf("create_qp_ex created a queue pair of type %v on context %#x, want RC on %p", q.Type(), q.qp.Context, ctx.ctx)
	}
}

// Without their ops, as a library that predates them or a context that is
// not a verbs_context lacks them, the XRC verbs report EOPNOTSUPP, which
// matches errors.ErrUnsupported, without a call into the provider.
func TestXRCWithoutTheOpsIsNotSupported(t *testing.T) {
	ctx, _ := fakeVerbsContext(t)
	cxrcd := (*rdmaabi.IbvXrcd)(cMemory(t, 64))
	cxrcd.Context = uint64(caddr(ctx))
	csrq := (*rdmaabi.IbvSrq)(cMemory(t, 64))
	csrq.Context = uint64(caddr(ctx))
	xrcd, srq := XRCDFromPointer(unsafe.Pointer(cxrcd)), SRQFromPointer(unsafe.Pointer(csrq))
	pd := PDFromPointer(cMemory(t, 64))
	for op, call := range map[string]func() error{
		"ibv_open_xrcd": func() error {
			_, err := ContextFromPointer(unsafe.Pointer(ctx)).OpenXRCD(-1, syscall.O_CREAT)
			return err
		},
		"ibv_close_xrcd":  func() error { return (&XRCD{xrcd: cxrcd, owned: true}).Close() },
		"ibv_get_srq_num": func() error { _, err := srq.Num(); return err },
		"ibv_create_srq_ex": func() error {
			_, err := pd.CreateSRQ(&SRQInitAttr{Type: SRQTypeXRC, Attr: SRQAttr{MaxWR: 1, MaxSGE: 1}, XRCD: xrcd, CQ: fakeCQ(t)})
			return err
		},
		"ibv_create_qp_ex": func() error {
			_, err := pd.CreateQP(&QPInitAttr{Type: QPTypeXRCRecv, XRCD: xrcd})
			return err
		},
	} {
		err := call()
		var opErr *OpError
		if !errors.As(err, &opErr) || opErr.Op != op || !errors.Is(err, syscall.EOPNOTSUPP) || !errors.Is(err, errors.ErrUnsupported) {
			t.Errorf("%s without its op: error = %v, want an *OpError of %s with EOPNOTSUPP", op, err, op)
		}
	}
}

// A wrapper of an XRC domain made elsewhere closes nothing, without the
// library or the provider.
func TestXRCDFromPointerCloseClosesNothing(t *testing.T) {
	useLibrary(t, missingLibrary)
	ctx, _ := fakeVerbsContext(t)
	cxrcd := (*rdmaabi.IbvXrcd)(cMemory(t, 64))
	cxrcd.Context = uint64(caddr(ctx))
	xrcd := XRCDFromPointer(unsafe.Pointer(cxrcd))
	for i := range 2 {
		if err := xrcd.Close(); err != nil {
			t.Errorf("Close() %d error = %v, want nil", i+1, err)
		}
	}
	if xrcd.Pointer() != nil {
		t.Errorf("Pointer() after Close() = %p, want nil", xrcd.Pointer())
	}
}
