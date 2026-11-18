//go:build linux && !android && (amd64 || arm64 || loong64 || ppc64le || riscv64 || s390x)

package ucma

import (
	"bytes"
	"net/netip"
	"slices"
	"testing"

	"github.com/wzshiming/rdma/internal/rdmaabi"
)

// A path record of the tests, each field set to a value that shows where its
// bits go.
var testPath = PathData{
	Flags: PathGMP | PathPrimary | PathBidirectional,
	Path: PathRecord{
		ServiceID:              0x0000000001061234,
		DGID:                   netip.MustParseAddr("fe80::248a:703:49:d4f1"),
		SGID:                   netip.MustParseAddr("fe80::248a:703:49:d4f0"),
		DLID:                   0x0012,
		SLID:                   0x0034,
		RawTraffic:             true,
		FlowLabel:              0xabcde,
		HopLimit:               64,
		TrafficClass:           0xb8,
		Reversible:             true,
		NumPath:                1,
		PKey:                   0xffff,
		QoSClass:               0xabc,
		SL:                     5,
		MTUSelector:            2,
		MTU:                    4,
		RateSelector:           2,
		Rate:                   7,
		PacketLifeTimeSelector: 2,
		PacketLifeTime:         0x12,
		Preference:             0x34,
	},
}

// testPathWire is testPath as struct ibv_path_data: the flags in the host's
// byte order, the record big-endian with the bits of each field where the
// kernel's table of the path record (ib_sa_pack_path) puts them.
func testPathWire() rdmaabi.IbvPathData {
	return rdmaabi.IbvPathData{
		Flags: 0x2b, // GMP, primary, outbound, inbound reverse
		Path: rdmaabi.IbvPathRecord{
			Service_id:         [8]byte{0, 0, 0, 0, 0x01, 0x06, 0x12, 0x34},
			Dgid:               netip.MustParseAddr("fe80::248a:703:49:d4f1").As16(),
			Sgid:               netip.MustParseAddr("fe80::248a:703:49:d4f0").As16(),
			Dlid:               [2]byte{0x00, 0x12},
			Slid:               [2]byte{0x00, 0x34},
			Flowlabel_hoplimit: [4]byte{0x8a, 0xbc, 0xde, 0x40}, // raw traffic in bit 31, flow label in 27-8, hop limit in 7-0
			Tclass:             0xb8,
			Reversible_numpath: 0x81, // reversible in bit 7, the number of paths in 6-0
			Pkey:               [2]byte{0xff, 0xff},
			Qosclass_sl:        [2]byte{0xab, 0xc5}, // QoS class in bits 15-4, SL in 3-0
			Mtu:                0x84,                // the selector in bits 7-6, the value in 5-0, as in the next two
			Rate:               0x87,
			Packetlifetime:     0x92,
			Preference:         0x34,
		},
	}
}

// An alternate path of the tests, which the kernel reports with the flags of
// a primary one.
var testAltPath = func() PathData {
	p := testPath
	p.Path.DLID = 0x0056
	return p
}()

func testAltPathWire() rdmaabi.IbvPathData {
	w := testPathWire()
	w.Path.Dlid = [2]byte{0x00, 0x56}
	return w
}

// pathValue is the memory of records one after the other, the value of the
// path option and the tail of the path query's response.
func pathValue(records ...rdmaabi.IbvPathData) []byte {
	var b []byte
	for _, r := range records {
		b = append(b, bytesOf(&r)...)
	}
	return b
}

func TestPathDataEncoding(t *testing.T) {
	if got, want := testPath.abi(), testPathWire(); got != want {
		t.Errorf("abi() = %+v\nwant %+v", got, want)
	}
}

func TestPathDataDecoding(t *testing.T) {
	wire := testPathWire()
	if got := pathData(&wire); got != testPath {
		t.Errorf("pathData() = %+v\nwant %+v", got, testPath)
	}
}

// Every bit of the record but the reserved ones belongs to a field: a record
// with all of them set decodes into each field's widest value and encodes
// back the same.
func TestPathRecordFieldsKeepTheirBits(t *testing.T) {
	var wire rdmaabi.IbvPathData
	b := bytesOf(&wire)
	copy(b, bytes.Repeat([]byte{0xff}, len(b)))
	clear(b[4:8])   // reserved
	b[52] &^= 0x70  // reserved: bits 30-28 of the flow label's word
	clear(b[66:72]) // reserved
	ones := netip.AddrFrom16([16]byte(bytes.Repeat([]byte{0xff}, 16)))
	want := PathData{Flags: 0xffffffff, Path: PathRecord{
		ServiceID: 0xffffffffffffffff, DGID: ones, SGID: ones, DLID: 0xffff, SLID: 0xffff,
		RawTraffic: true, FlowLabel: 0xfffff, HopLimit: 0xff, TrafficClass: 0xff, Reversible: true, NumPath: 0x7f,
		PKey: 0xffff, QoSClass: 0xfff, SL: 0xf, MTUSelector: 3, MTU: 0x3f, RateSelector: 3, Rate: 0x3f,
		PacketLifeTimeSelector: 3, PacketLifeTime: 0x3f, Preference: 0xff,
	}}
	got := pathData(&wire)
	if got != want {
		t.Errorf("pathData() of a record with every bit set = %+v\nwant %+v", got, want)
	}
	if back := got.abi(); !bytes.Equal(bytesOf(&back), b) {
		t.Errorf("abi() of it = % x\nwant % x", bytesOf(&back), b)
	}
}

// The response to the path query is the number of paths the route has,
// none before it is resolved, and that many struct ibv_path_data.
func TestQueryPathDecodesTheRecords(t *testing.T) {
	for _, tt := range []struct {
		name    string
		records []rdmaabi.IbvPathData
		want    []PathData
	}{
		{"no route", nil, nil},
		{"a primary path", []rdmaabi.IbvPathData{testPathWire()}, []PathData{testPath}},
		{"a primary and an alternate path", []rdmaabi.IbvPathData{testPathWire(), testAltPathWire()}, []PathData{testPath, testAltPath}},
	} {
		d := newFakeDevice(t)
		hdr := rdmaabi.UcmaAbiQueryPathResp{Num_paths: uint32(len(tt.records))}
		d.answer(append(slices.Clone(bytesOf(&hdr)), pathValue(tt.records...)...))
		got, err := d.ch.QueryPath(7)
		if !slices.Equal(got, tt.want) || err != nil {
			t.Errorf("%s: QueryPath() = %+v, %v; want %+v", tt.name, got, err, tt.want)
		}
	}
}
