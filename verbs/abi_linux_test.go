//go:build linux && !android && (amd64 || arm64 || loong64 || ppc64le || riscv64 || s390x)

package verbs

import (
	"encoding/binary"
	"os"
	"syscall"
	"testing"
	"unsafe"

	"github.com/wzshiming/rdma/internal/rdmaabi"
)

// Immediate data travels in network byte order: 0x01020304 is the bytes
// 01 02 03 04 of the work request, and those bytes of a completion read back
// as 0x01020304.
func TestSendWRImmIsNetworkOrder(t *testing.T) {
	var cw rdmaabi.IbvSendWr
	sendWRToC(&cw, &SendWR{ID: 7, Op: WRSendImm, Imm: 0x01020304})
	if cw.Imm_data != [4]byte{1, 2, 3, 4} {
		t.Errorf("imm_data = % x, want 01 02 03 04", cw.Imm_data)
	}
	if cw.Wr_id != 7 || cw.Opcode != uint32(WRSendImm) {
		t.Errorf("wr_id, opcode = %d, %d; want 7, %d", cw.Wr_id, cw.Opcode, WRSendImm)
	}
}

func TestWCImmIsNetworkOrder(t *testing.T) {
	wc := wcFromC(&rdmaabi.IbvWc{Wr_id: 3, Imm_data: [4]byte{1, 2, 3, 4}, Wc_flags: uint32(WCWithImm), Opcode: uint32(WCOpRecv)})
	if !wc.HasImm() || wc.Imm != 0x01020304 {
		t.Errorf("WC = %+v, want imm 0x01020304", wc)
	}
	if wc.ID != 3 || wc.Opcode != WCOpRecv {
		t.Errorf("WC = %+v, want id 3, RECV", wc)
	}
}

func TestGUIDFromBigEndianBytes(t *testing.T) {
	b := [8]byte{0x02, 0x50, 0xb6, 0xff, 0xfe, 0x0a, 0x4d, 0x8c}
	const want = 0x0250b6fffe0a4d8c
	if got := guid(b); got != want {
		t.Errorf("guid(% x) = %#x, want %#x", b, got, want)
	}
	// ibv_get_device_guid hands the same bytes back loaded into a register.
	if got := be64(binary.NativeEndian.Uint64(b[:])); got != want {
		t.Errorf("be64(register of % x) = %#x, want %#x", b, got, want)
	}
}

func TestGIDEntryKeepsTheBytes(t *testing.T) {
	gid := rdmaabi.IbvGid{0: 0xfe, 1: 0x80, 14: 0xab, 15: 0x01}
	got := gidEntryFromC(&rdmaabi.IbvGidEntry{Gid: gid, Gid_index: 3, Port_num: 1, Gid_type: 2})
	want := GIDEntry{Index: 3, Type: GIDTypeRoCEv2, GID: GID{0: 0xfe, 1: 0x80, 14: 0xab, 15: 0x01}}
	if got != want {
		t.Errorf("gidEntryFromC() = %+v, want %+v", got, want)
	}
	if got.GID.String() != "fe80::ab01" {
		t.Errorf("GID = %v, want fe80::ab01", got.GID)
	}
}

func TestAHAttrRoundTripsThroughC(t *testing.T) {
	a := AHAttr{
		GRH:         GlobalRoute{DGID: GID{0: 0xfe, 1: 0x80, 15: 0x01}, FlowLabel: 7, SGIDIndex: 1, HopLimit: 64, TrafficClass: 2},
		DLID:        5,
		SL:          1,
		SrcPathBits: 2,
		StaticRate:  3,
		IsGlobal:    true,
		Port:        1,
	}
	ca := ahToC(a)
	if ca.Grh.Dgid != rdmaabi.IbvGid(a.GRH.DGID) {
		t.Errorf("dgid = % x, want % x", ca.Grh.Dgid, a.GRH.DGID)
	}
	if got := ahFromC(&ca); got != a {
		t.Errorf("ahFromC(ahToC(a)) = %+v, want %+v", got, a)
	}
}

// cMemory returns size zeroed bytes outside the Go heap, where the library's
// own structures live, unmapped when the test ends.
func cMemory(t *testing.T, size int) unsafe.Pointer {
	t.Helper()
	b, err := syscall.Mmap(-1, 0, size, syscall.PROT_READ|syscall.PROT_WRITE, syscall.MAP_ANON|syscall.MAP_PRIVATE)
	if err != nil {
		t.Fatalf("mmap: %v", err)
	}
	t.Cleanup(func() { syscall.Munmap(b) })
	return unsafe.Pointer(unsafe.SliceData(b))
}

// The transport type is the enum ibv_transport_type libibverbs stores in
// struct ibv_device next to the node type it derives it from; a context
// reaches that struct through its device member.
func TestTransportTypeReadsStructIbvDevice(t *testing.T) {
	cdev := (*rdmaabi.IbvDevice)(cMemory(t, os.Getpagesize()))
	cctx := (*rdmaabi.IbvContext)(cMemory(t, os.Getpagesize()))
	cctx.Device = uint64(uintptr(unsafe.Pointer(cdev)))
	dev := &Device{dev: uintptr(unsafe.Pointer(cdev))}
	ctx := ContextFromPointer(unsafe.Pointer(cctx))
	tests := []struct {
		node, transport int32 // enum ibv_node_type, enum ibv_transport_type
		want            TransportType
	}{
		{1, 0, TransportIB},          // IBV_NODE_CA, IBV_TRANSPORT_IB: InfiniBand and RoCE
		{4, 1, TransportIWARP},       // IBV_NODE_RNIC, IBV_TRANSPORT_IWARP
		{5, 2, TransportUSNIC},       // IBV_NODE_USNIC, IBV_TRANSPORT_USNIC
		{6, 3, TransportUSNICUDP},    // IBV_NODE_USNIC_UDP, IBV_TRANSPORT_USNIC_UDP
		{7, 4, TransportUnspecified}, // IBV_NODE_UNSPECIFIED, IBV_TRANSPORT_UNSPECIFIED
		{-1, -1, TransportUnknown},   // IBV_NODE_UNKNOWN, IBV_TRANSPORT_UNKNOWN
	}
	for _, tt := range tests {
		cdev.Node_type, cdev.Transport_type = tt.node, tt.transport
		if got := dev.TransportType(); got != tt.want {
			t.Errorf("Device.TransportType() with transport_type %d = %v, want %v", tt.transport, got, tt.want)
		}
		if got := ctx.TransportType(); got != tt.want {
			t.Errorf("Context.TransportType() with transport_type %d = %v, want %v", tt.transport, got, tt.want)
		}
	}
}
