//go:build linux && !android && (amd64 || arm64 || loong64 || ppc64le || riscv64 || s390x)

package ucma

import (
	"encoding/binary"
	"fmt"
	"net/netip"
	"syscall"
	"unsafe"

	"github.com/wzshiming/rdma/internal/rdmaabi"
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

// afIB is AF_IB, the family of struct sockaddr_ib.
const afIB = 27

// IBAddr is an InfiniBand address, the struct sockaddr_ib of the family
// AF_IB. The zero IBAddr, without a GID, is no address.
type IBAddr struct {
	// GID is the GID, written like an IPv6 address; an IPv4 address stands
	// for its v4-mapped form, the GID RoCE gives it, and a zone is dropped.
	GID      netip.Addr
	PKey     uint16 // the partition key
	FlowInfo uint32 // the flow information, as in sin6_flowinfo
	// ServiceID is the service ID and ServiceIDMask the bits of it that
	// count. For the port spaces of IP the kernel reads the port space in
	// bits 16 to 31 (RDMA_IB_IP_PS_TCP, RDMA_IB_IP_PS_UDP) and the port in
	// bits 0 to 15, port 0 taking a free one.
	ServiceID, ServiceIDMask uint64
	ScopeID                  uint64 // the scope, as in sin6_scope_id
}

// sockaddr returns the struct sockaddr_storage of a and the size of the
// struct sockaddr_ib in it.
func (a IBAddr) sockaddr() (ss [128]byte, size uint16, err error) {
	if !a.GID.IsValid() {
		return ss, 0, fmt.Errorf("ucma: InfiniBand address %+v has no GID: %w", a, syscall.EINVAL)
	}
	sib := rdmaabi.SockaddrIb{Sib_family: afIB, Sib_addr: a.GID.As16(), Sib_scope_id: a.ScopeID}
	binary.BigEndian.PutUint16(sib.Sib_pkey[:], a.PKey)
	binary.BigEndian.PutUint32(sib.Sib_flowinfo[:], a.FlowInfo)
	binary.BigEndian.PutUint64(sib.Sib_sid[:], a.ServiceID)
	binary.BigEndian.PutUint64(sib.Sib_sid_mask[:], a.ServiceIDMask)
	copy(ss[:], unsafe.Slice((*byte)(unsafe.Pointer(&sib)), unsafe.Sizeof(sib)))
	return ss, uint16(unsafe.Sizeof(sib)), nil
}

// ibAddr reads the InfiniBand address of a struct sockaddr_storage; another
// family gives the zero IBAddr.
func ibAddr(ss *[128]byte) IBAddr {
	var sib rdmaabi.SockaddrIb
	copy(unsafe.Slice((*byte)(unsafe.Pointer(&sib)), unsafe.Sizeof(sib)), ss[:])
	if sib.Sib_family != afIB {
		return IBAddr{}
	}
	return IBAddr{
		GID:           netip.AddrFrom16(sib.Sib_addr),
		PKey:          binary.BigEndian.Uint16(sib.Sib_pkey[:]),
		FlowInfo:      binary.BigEndian.Uint32(sib.Sib_flowinfo[:]),
		ServiceID:     binary.BigEndian.Uint64(sib.Sib_sid[:]),
		ServiceIDMask: binary.BigEndian.Uint64(sib.Sib_sid_mask[:]),
		ScopeID:       sib.Sib_scope_id,
	}
}
