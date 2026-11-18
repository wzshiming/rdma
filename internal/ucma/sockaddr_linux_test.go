//go:build linux && !android && (amd64 || arm64 || loong64 || ppc64le || riscv64 || s390x)

package ucma

import (
	"encoding/binary"
	"errors"
	"net/netip"
	"syscall"
	"testing"

	"github.com/wzshiming/rdma/internal/rdmaabi"
)

// Addresses convert as package cm converts them for librdmacm: IPv4 and
// v4-mapped addresses as struct sockaddr_in, the others as struct
// sockaddr_in6, the port in network order, and no zone: sin6_flowinfo and
// sin6_scope_id stay 0.
func TestSockaddrEncoding(t *testing.T) {
	for _, tt := range []struct {
		in   string
		want [128]byte
		size uint16
	}{
		{"10.200.0.1:4791", sockaddrIn("10.200.0.1", 4791), 16},
		{"[2001:db8::1]:7471", sockaddrIn6("2001:db8::1", 7471), 28},
		{"[::ffff:10.200.0.1]:4791", sockaddrIn("10.200.0.1", 4791), 16},
		{"[fe80::1%eth0]:7471", sockaddrIn6("fe80::1", 7471), 28},
		{"0.0.0.0:0", sockaddrIn("0.0.0.0", 0), 16},
		{"[::]:0", sockaddrIn6("::", 0), 28},
	} {
		ss, size, err := sockaddr(netip.MustParseAddrPort(tt.in))
		if ss != tt.want || size != tt.size || err != nil {
			t.Errorf("sockaddr(%s) = % x (%d bytes), %v; want % x (%d bytes)", tt.in, ss[:28], size, err, tt.want[:28], tt.size)
		}
	}
	if _, _, err := sockaddr(netip.AddrPort{}); !errors.Is(err, syscall.EINVAL) {
		t.Errorf("sockaddr(zero AddrPort) error = %v, want EINVAL", err)
	}
}

func TestSockaddrDecoding(t *testing.T) {
	for _, tt := range []struct {
		ss   [128]byte
		want string
	}{
		{sockaddrIn("10.200.0.1", 4791), "10.200.0.1:4791"},
		{sockaddrIn6("2001:db8::2", 7471), "[2001:db8::2]:7471"},
		{sockaddrIn6("::ffff:10.200.0.1", 4791), "[::ffff:10.200.0.1]:4791"},
		{sockaddrIn("0.0.0.0", 0), "0.0.0.0:0"},
	} {
		if got := addrPort(&tt.ss); got != netip.MustParseAddrPort(tt.want) {
			t.Errorf("addrPort(% x) = %v, want %s", tt.ss[:28], got, tt.want)
		}
	}
	scoped := sockaddrIn6("fe80::1", 7471)
	binary.NativeEndian.PutUint32(scoped[24:], 2)
	if got := addrPort(&scoped); got != netip.MustParseAddrPort("[fe80::1]:7471") {
		t.Errorf("addrPort of a scoped sockaddr_in6 = %v, want [fe80::1]:7471 without a zone", got)
	}
	var unset, unix [128]byte
	binary.NativeEndian.PutUint16(unix[0:], syscall.AF_UNIX)
	for _, ss := range [][128]byte{unset, unix} {
		if got := addrPort(&ss); got.IsValid() {
			t.Errorf("addrPort of family %d = %v, want the zero AddrPort", binary.NativeEndian.Uint16(ss[:]), got)
		}
	}
}

// An InfiniBand address of the tests: a GID of a port, a partition key with
// the full membership bit, and a service ID of the TCP port space with its
// port, every bit of which counts.
var testIBAddr = IBAddr{
	GID:           netip.MustParseAddr("fe80::248a:703:49:d4f0"),
	PKey:          0x8001,
	FlowInfo:      0x12345,
	ServiceID:     0x0000000001061234,
	ServiceIDMask: 0xffffffffffffffff,
	ScopeID:       3,
}

// sockaddrIb builds the struct sockaddr_storage of an InfiniBand address the
// way the kernel reads it: a struct sockaddr_ib whose family AF_IB and scope
// id are in the host's byte order, the rest in network order.
func sockaddrIb(gid string, pkey uint16, flowInfo uint32, sid, sidMask, scopeID uint64) (ss [128]byte) {
	sib := rdmaabi.SockaddrIb{Sib_family: 27, Sib_addr: netip.MustParseAddr(gid).As16(), Sib_scope_id: scopeID}
	binary.BigEndian.PutUint16(sib.Sib_pkey[:], pkey)
	binary.BigEndian.PutUint32(sib.Sib_flowinfo[:], flowInfo)
	binary.BigEndian.PutUint64(sib.Sib_sid[:], sid)
	binary.BigEndian.PutUint64(sib.Sib_sid_mask[:], sidMask)
	copy(ss[:], bytesOf(&sib))
	return ss
}

// An InfiniBand address is a struct sockaddr_ib of 48 bytes. Its GID is a
// 16-byte address: an IPv4 address goes in its v4-mapped form, the GID
// RoCE gives it, and a zone is dropped.
func TestSockaddrIBEncoding(t *testing.T) {
	for _, tt := range []struct {
		name string
		in   IBAddr
		want [128]byte
	}{
		{"every field", testIBAddr, sockaddrIb("fe80::248a:703:49:d4f0", 0x8001, 0x12345, 0x0000000001061234, 0xffffffffffffffff, 3)},
		{"the any address", IBAddr{GID: netip.IPv6Unspecified(), ServiceID: 0x0000000001060000, ServiceIDMask: 0xffffffffffff0000},
			sockaddrIb("::", 0, 0, 0x0000000001060000, 0xffffffffffff0000, 0)},
		{"an IPv4 GID", IBAddr{GID: netip.MustParseAddr("10.200.0.1"), PKey: 0xffff}, sockaddrIb("::ffff:10.200.0.1", 0xffff, 0, 0, 0, 0)},
		{"a zone", IBAddr{GID: netip.MustParseAddr("fe80::1%ib0")}, sockaddrIb("fe80::1", 0, 0, 0, 0, 0)},
	} {
		ss, size, err := tt.in.sockaddr()
		if ss != tt.want || size != 48 || err != nil {
			t.Errorf("%s: sockaddr() = % x (%d bytes), %v; want % x (48 bytes)", tt.name, ss[:48], size, err, tt.want[:48])
		}
	}
	if _, _, err := (IBAddr{PKey: 0xffff}).sockaddr(); !errors.Is(err, syscall.EINVAL) {
		t.Errorf("sockaddr() of an IBAddr without a GID: error %v, want EINVAL", err)
	}
}

func TestSockaddrIBDecoding(t *testing.T) {
	ss := sockaddrIb("fe80::248a:703:49:d4f0", 0x8001, 0x12345, 0x0000000001061234, 0xffffffffffffffff, 3)
	if got := ibAddr(&ss); got != testIBAddr {
		t.Errorf("ibAddr(% x) = %+v, want %+v", ss[:48], got, testIBAddr)
	}
	mapped := sockaddrIb("::ffff:10.200.0.1", 0xffff, 0, 0x0000000001061234, 0, 0)
	if got, want := ibAddr(&mapped).GID, netip.MustParseAddr("::ffff:10.200.0.1"); got != want {
		t.Errorf("ibAddr of a v4-mapped GID: GID %v, want %v, a GID of 16 bytes", got, want)
	}
	for _, ss := range [][128]byte{sockaddrIn("10.200.0.1", 4791), sockaddrIn6("2001:db8::2", 7471), {}} {
		if got := ibAddr(&ss); got != (IBAddr{}) {
			t.Errorf("ibAddr of family %d = %+v, want the zero IBAddr", binary.NativeEndian.Uint16(ss[:]), got)
		}
	}
}
