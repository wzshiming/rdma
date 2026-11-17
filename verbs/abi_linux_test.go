//go:build linux && !android && (amd64 || arm64 || loong64 || ppc64le || riscv64 || s390x)

package verbs

import (
	"bytes"
	"encoding/binary"
	"errors"
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

// requestBytes returns the 128 bytes of struct ibv_send_wr that cw is.
func requestBytes(cw *rdmaabi.IbvSendWr) []byte {
	return unsafe.Slice((*byte)(unsafe.Pointer(cw)), unsafe.Sizeof(*cw))
}

// The request of a send on a connected queue pair, as the package built it
// before datagrams: its fields and the rdma member of the wr union at their
// C offsets, the chain's fields and the rest zero.
func TestRCSendRequestBytes(t *testing.T) {
	var cw rdmaabi.IbvSendWr
	sendWRToC(&cw, &SendWR{ID: 0x0102030405060708, Op: WRWriteImm, Flags: SendSignaled | SendSolicited, Imm: 0xa1b2c3d4, RemoteAddr: 0x1112131415161718, RKey: 0x21222324})
	want := make([]byte, 128)
	binary.NativeEndian.PutUint64(want[0:], 0x0102030405060708)  // wr_id
	binary.NativeEndian.PutUint32(want[28:], 1)                  // opcode IBV_WR_RDMA_WRITE_WITH_IMM
	binary.NativeEndian.PutUint32(want[32:], 2|4)                // send_flags IBV_SEND_SIGNALED|IBV_SEND_SOLICITED
	copy(want[36:], []byte{0xa1, 0xb2, 0xc3, 0xd4})              // imm_data
	binary.NativeEndian.PutUint64(want[40:], 0x1112131415161718) // wr.rdma.remote_addr
	binary.NativeEndian.PutUint32(want[48:], 0x21222324)         // wr.rdma.rkey
	if got := requestBytes(&cw); !bytes.Equal(got, want) {
		t.Errorf("request =\n% x\nwant\n% x", got, want)
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

// ahToC puts each field where struct ibv_ah_attr has it, and ahFromC reads
// it back from there.
func TestAHAttrRoundTripsThroughC(t *testing.T) {
	a := AHAttr{
		GRH:         GlobalRoute{DGID: GID{0: 0xfe, 1: 0x80, 15: 0x01}, FlowLabel: 0x12345, SGIDIndex: 8, HopLimit: 64, TrafficClass: 2},
		DLID:        5,
		SL:          3,
		SrcPathBits: 4,
		StaticRate:  6,
		IsGlobal:    true,
		Port:        7,
	}
	want := rdmaabi.IbvAhAttr{
		Grh:           rdmaabi.IbvGlobalRoute{Dgid: rdmaabi.IbvGid{0: 0xfe, 1: 0x80, 15: 0x01}, Flow_label: 0x12345, Sgid_index: 8, Hop_limit: 64, Traffic_class: 2},
		Dlid:          5,
		Sl:            3,
		Src_path_bits: 4,
		Static_rate:   6,
		Is_global:     1,
		Port_num:      7,
	}
	ca := ahToC(a)
	if ca != want {
		t.Errorf("ahToC(a) = %+v, want %+v", ca, want)
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

// fakeQP returns a queue pair of type typ in C memory, on a context whose ops
// table is empty: requests can be built for it, never posted.
func fakeQP(t *testing.T, typ QPType) *QP {
	t.Helper()
	cctx := (*rdmaabi.IbvContext)(cMemory(t, os.Getpagesize()))
	cqp := (*rdmaabi.IbvQp)(cMemory(t, os.Getpagesize()))
	cqp.Context = uint64(uintptr(unsafe.Pointer(cctx)))
	cqp.Qp_type = uint32(typ)
	return QPFromPointer(unsafe.Pointer(cqp))
}

// A send of a UD queue pair carries its destination where other requests
// carry their RDMA address: the address handle, the remote queue pair number
// and qkey in the ud member of the wr union.
func TestDatagramSendRequestBytes(t *testing.T) {
	ah := AHFromPointer(cMemory(t, 64))
	w := SendWR{ID: 0x0102030405060708, Op: WRSendImm, Flags: SendSignaled, Imm: 0xa1b2c3d4, RemoteAddr: 0x1112131415161718, RKey: 0x21222324, AH: ah, RemoteQPN: 0x00abcdef, RemoteQKey: 0x01234567}
	var cw rdmaabi.IbvSendWr
	if err := fakeQP(t, QPTypeUD).requestToC(&cw, &w); err != nil {
		t.Fatalf("requestToC() error: %v", err)
	}
	want := make([]byte, 128)
	binary.NativeEndian.PutUint64(want[0:], 0x0102030405060708)             // wr_id
	binary.NativeEndian.PutUint32(want[28:], 3)                             // opcode IBV_WR_SEND_WITH_IMM
	binary.NativeEndian.PutUint32(want[32:], 2)                             // send_flags IBV_SEND_SIGNALED
	copy(want[36:], []byte{0xa1, 0xb2, 0xc3, 0xd4})                         // imm_data
	binary.NativeEndian.PutUint64(want[40:], uint64(uintptr(ah.Pointer()))) // wr.ud.ah
	binary.NativeEndian.PutUint32(want[48:], 0x00abcdef)                    // wr.ud.remote_qpn
	binary.NativeEndian.PutUint32(want[52:], 0x01234567)                    // wr.ud.remote_qkey
	if got := requestBytes(&cw); !bytes.Equal(got, want) {
		t.Errorf("request =\n% x\nwant\n% x", got, want)
	}
}

// The datagram fields of a request change nothing on a connected queue pair.
func TestRCSendRequestIgnoresDatagramFields(t *testing.T) {
	w := SendWR{ID: 0x0102030405060708, Op: WRWriteImm, Flags: SendSignaled | SendSolicited, Imm: 0xa1b2c3d4, RemoteAddr: 0x1112131415161718, RKey: 0x21222324}
	var want rdmaabi.IbvSendWr
	sendWRToC(&want, &w)
	w.AH, w.RemoteQPN, w.RemoteQKey = AHFromPointer(cMemory(t, 64)), 0x00abcdef, 0x01234567
	var got rdmaabi.IbvSendWr
	if err := fakeQP(t, QPTypeRC).requestToC(&got, &w); err != nil {
		t.Fatalf("requestToC() error: %v", err)
	}
	if !bytes.Equal(requestBytes(&got), requestBytes(&want)) {
		t.Errorf("request =\n% x\nwant\n% x", requestBytes(&got), requestBytes(&want))
	}
}

// A UD queue pair refuses a datagram without an address handle before the
// provider follows the null pointer, and posts none of the chain: the post
// function of a fake queue pair is at address zero.
func TestDatagramWithoutAHIsInvalid(t *testing.T) {
	ah := AHFromPointer(cMemory(t, 64))
	err := fakeQP(t, QPTypeUD).PostSend(SendWR{ID: 1, Op: WRSend, AH: ah}, SendWR{ID: 2, Op: WRSend})
	var opErr *OpError
	if !errors.As(err, &opErr) || opErr.Op != "ibv_post_send" || !errors.Is(err, syscall.EINVAL) {
		t.Errorf("PostSend() of a datagram without an address handle error = %v, want an *OpError of ibv_post_send matching EINVAL", err)
	}
}

// A closed address handle refuses to be sent to, as a closed CQ refuses to
// be polled.
func TestDatagramToAClosedAHIsClosed(t *testing.T) {
	ah := AHFromPointer(cMemory(t, 64))
	if err := ah.Close(); err != nil {
		t.Fatalf("Close() error: %v", err)
	}
	err := fakeQP(t, QPTypeUD).PostSend(SendWR{ID: 1, Op: WRSend, AH: ah})
	var opErr *OpError
	if !errors.As(err, &opErr) || opErr.Op != "ibv_post_send" || !errors.Is(err, os.ErrClosed) {
		t.Errorf("PostSend() to a closed address handle error = %v, want an *OpError of ibv_post_send matching os.ErrClosed", err)
	}
}
