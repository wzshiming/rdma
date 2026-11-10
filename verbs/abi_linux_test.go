//go:build linux && !android && (amd64 || arm64 || loong64 || ppc64le || riscv64 || s390x)

package verbs

import (
	"encoding/binary"
	"testing"

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
