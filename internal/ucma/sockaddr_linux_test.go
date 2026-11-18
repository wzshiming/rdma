//go:build linux && !android && (amd64 || arm64 || loong64 || ppc64le || riscv64 || s390x)

package ucma

import (
	"encoding/binary"
	"errors"
	"net/netip"
	"syscall"
	"testing"
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
