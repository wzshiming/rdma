//go:build linux && !android && (amd64 || arm64 || loong64 || ppc64le || riscv64 || s390x)

package ucma

import (
	"encoding/binary"
	"fmt"
	"net/netip"
	"syscall"
)

// sockaddr returns the struct sockaddr_storage of ap and the size of the
// sockaddr in it, built as package cm builds it for librdmacm: an IPv4 or
// v4-mapped address becomes a struct sockaddr_in, another a struct
// sockaddr_in6; the port is in network order; the zone is dropped, and
// sin6_flowinfo and sin6_scope_id stay 0.
func sockaddr(ap netip.AddrPort) (ss [128]byte, size uint16, err error) {
	addr := ap.Addr().Unmap()
	if !addr.IsValid() {
		return ss, 0, fmt.Errorf("ucma: address %v has no IP: %w", ap, syscall.EINVAL)
	}
	binary.BigEndian.PutUint16(ss[2:], ap.Port())
	if addr.Is4() {
		binary.NativeEndian.PutUint16(ss[0:], syscall.AF_INET)
		ip := addr.As4()
		copy(ss[4:], ip[:])
		return ss, syscall.SizeofSockaddrInet4, nil
	}
	binary.NativeEndian.PutUint16(ss[0:], syscall.AF_INET6)
	ip := addr.As16()
	copy(ss[8:], ip[:])
	return ss, syscall.SizeofSockaddrInet6, nil
}

// addrPort reads the IPv4 or IPv6 address of a struct sockaddr_storage,
// without a zone; another family gives the zero AddrPort.
func addrPort(ss *[128]byte) netip.AddrPort {
	port := binary.BigEndian.Uint16(ss[2:])
	switch binary.NativeEndian.Uint16(ss[0:]) {
	case syscall.AF_INET:
		return netip.AddrPortFrom(netip.AddrFrom4([4]byte(ss[4:8])), port)
	case syscall.AF_INET6:
		return netip.AddrPortFrom(netip.AddrFrom16([16]byte(ss[8:24])), port)
	}
	return netip.AddrPort{}
}
