//go:build linux && !android && (amd64 || arm64 || loong64 || ppc64le || riscv64 || s390x)

package rsocket

import (
	"net/netip"
	"syscall"
	"testing"
	"unsafe"
)

// bytesOf returns the memory of ss.
func bytesOf(ss *sockaddrStorage) []byte {
	return unsafe.Slice((*byte)(unsafe.Pointer(ss)), unsafe.Sizeof(*ss))
}

// TestSockaddrPortIsNetworkOrder checks the sockaddr handed to librdmacm:
// the family as the host's sa_family_t, the port in network order whatever
// the host's byte order, and the address bytes after them.
func TestSockaddrPortIsNetworkOrder(t *testing.T) {
	var ss sockaddrStorage
	if n := ss.set(netip.MustParseAddrPort("10.200.0.1:7471")); n != 16 {
		t.Fatalf("set(IPv4) = %d, want sizeof(struct sockaddr_in) 16", n)
	}
	raw := bytesOf(&ss)
	if ss.family != syscall.AF_INET || raw[2] != 0x1d || raw[3] != 0x2f || string(raw[4:8]) != "\x0a\xc8\x00\x01" {
		t.Errorf("sockaddr_in = % x, want family AF_INET, port 1d 2f, address 0a c8 00 01", raw[:16])
	}
	if n := ss.set(netip.MustParseAddrPort("[2001:db8::1]:4791")); n != 28 {
		t.Fatalf("set(IPv6) = %d, want sizeof(struct sockaddr_in6) 28", n)
	}
	raw = bytesOf(&ss)
	if ss.family != syscall.AF_INET6 || raw[2] != 0x12 || raw[3] != 0xb7 || string(raw[8:24]) != "\x20\x01\x0d\xb8"+"\x00\x00\x00\x00\x00\x00\x00\x00\x00\x00\x00\x01" {
		t.Errorf("sockaddr_in6 = % x, want family AF_INET6, port 12 b7, address 2001:db8::1 at offset 8", raw[:28])
	}
}

func TestSockaddrRoundTrip(t *testing.T) {
	for _, tt := range []struct {
		in, want string
	}{
		{"10.200.0.1:7471", "10.200.0.1:7471"},
		{"[2001:db8::1]:4791", "[2001:db8::1]:4791"},
		{"[::ffff:10.200.0.1]:7471", "10.200.0.1:7471"}, // v4-mapped becomes AF_INET
		{"[::]:0", "[::]:0"},
	} {
		var ss sockaddrStorage
		ss.set(netip.MustParseAddrPort(tt.in))
		if got := ss.addrPort(); got != netip.MustParseAddrPort(tt.want) {
			t.Errorf("addrPort(set(%s)) = %v, want %s", tt.in, got, tt.want)
		}
	}
	var ss sockaddrStorage
	ss.family = syscall.AF_UNIX
	if got := ss.addrPort(); got.IsValid() {
		t.Errorf("addrPort() of an AF_UNIX sockaddr = %v, want the zero value", got)
	}
}
