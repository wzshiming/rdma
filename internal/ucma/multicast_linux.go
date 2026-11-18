//go:build linux && !android && (amd64 || arm64 || loong64 || ppc64le || riscv64 || s390x)

package ucma

import (
	"net/netip"

	"github.com/wzshiming/rdma/internal/rdmaabi"
)

// The multicast commands, numbered as in rdma/rdma_cma_abi.h.
const (
	cmdLeaveMcast = 17
	cmdJoinMcast  = 22
)

// JoinFlags is the kind of a multicast membership (RDMA_MC_JOIN_FLAG_*).
type JoinFlags uint16

// Kinds of membership.
const (
	JoinFullMember         JoinFlags = 0 // sends to the group and receives from it
	JoinSendOnlyFullMember JoinFlags = 1 // sends to the group only
)

// JoinMulticast joins the multicast group at the address group on behalf of
// an identifier of the UD type that is bound to a device, by Bind to a
// local address or by ResolveAddr, and returns the id of the membership. The
// kernel refuses another identifier with EINVAL.
//
// The join ends in an event whose UID is uid and whose ID is the
// membership's id. EventMulticastJoin carries the group in its datagram
// parameters (Event.UD): the address-handle attributes that reach it, with
// the group's GID in AHAttr.Grh.Dgid and its LID in AHAttr.Dlid, the QKey
// of its datagrams and QPNum 0xFFFFFF, the multicast queue pair number; a
// queue pair receives from the group once attached to that GID and LID,
// which librdmacm does unless the membership sends only.
// EventMulticastError carries a negative errno in Status. The kernel drops
// both, like every event of the identifier, when the identifier's own uid
// is 0.
func (c *Channel) JoinMulticast(id uint32, uid uint64, group netip.Addr, flags JoinFlags) (uint32, error) {
	ss, size, err := sockaddr(netip.AddrPortFrom(group, 0))
	if err != nil {
		return 0, err
	}
	cmd := rdmaabi.UcmaAbiJoinMcast{Uid: uid, Id: id, Addr_size: size, Join_flags: uint16(flags), Addr: ss}
	var resp rdmaabi.UcmaAbiCreateIdResp
	if err := call(c, cmdJoinMcast, &cmd, &cmd.Response, &resp); err != nil {
		return 0, err
	}
	return resp.Id, nil
}

// LeaveMulticast ends the membership mcID with its queued events and
// returns how many of its events the channel has handed out.
func (c *Channel) LeaveMulticast(mcID uint32) (eventsReported uint32, err error) {
	cmd := rdmaabi.UcmaAbiDestroyId{Id: mcID}
	var resp rdmaabi.UcmaAbiDestroyIdResp
	if err := call(c, cmdLeaveMcast, &cmd, &cmd.Response, &resp); err != nil {
		return 0, err
	}
	return resp.Events_reported, nil
}
