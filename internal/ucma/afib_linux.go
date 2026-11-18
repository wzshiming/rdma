//go:build linux && !android && (amd64 || arm64 || loong64 || ppc64le || riscv64 || s390x)

package ucma

import "net/netip"

// ipPortSpaceMask covers the port space in the service ID of an InfiniBand
// address of the port spaces of IP (RDMA_IB_IP_PS_MASK), whose service IDs
// are the port space shifted by 16: RDMA_IB_IP_PS_TCP for PortSpaceTCP.
const ipPortSpaceMask = 0xFFFFFFFFFFFF0000

// probeUID is the uid of the probe's identifier, which has no events.
const probeUID = 1

// ProbeAFIB reports whether the kernel takes InfiniBand addresses, as
// librdmacm finds out at its start: an identifier of PortSpaceIB binds to
// the any address of AF_IB in the TCP port space and its addresses and GIDs
// are queried, as rdma_bind_addr does after a bind, and the identifier is
// destroyed. Any failure, CreateID's too, means no.
func (c *Channel) ProbeAFIB() bool {
	id, err := c.CreateID(probeUID, PortSpaceIB, QPTypeRC)
	if err != nil {
		return false
	}
	defer c.DestroyID(id)
	addr := IBAddr{GID: netip.IPv6Unspecified(), ServiceID: uint64(PortSpaceTCP) << 16, ServiceIDMask: ipPortSpaceMask}
	if err := c.BindIB(id, addr); err != nil {
		return false
	}
	if _, err := c.QueryAddr(id); err != nil {
		return false
	}
	_, err = c.QueryGID(id)
	return err == nil
}
