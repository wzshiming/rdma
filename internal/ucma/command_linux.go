//go:build linux && !android && (amd64 || arm64 || loong64 || ppc64le || riscv64 || s390x)

package ucma

import (
	"encoding/binary"
	"fmt"
	"math"
	"net/netip"
	"os"
	"syscall"
	"time"

	"github.com/wzshiming/rdma/internal/rdmaabi"
)

// The commands, numbered as in rdma/rdma_cma_abi.h.
const (
	cmdCreateID     = 0
	cmdDestroyID    = 1
	cmdResolveRoute = 4
	cmdConnect      = 6
	cmdListen       = 7
	cmdAccept       = 8
	cmdReject       = 9
	cmdDisconnect   = 10
	cmdInitQPAttr   = 11
	cmdGetEvent     = 12
	cmdMigrateID    = 18
	cmdQuery        = 19
	cmdBind         = 20
	cmdResolveAddr  = 21
)

// The options of a query (UCMA_QUERY_*).
const (
	queryAddr = 0
	queryPath = 1
	queryGID  = 2
)

// maxPrivateData is the most private data a request carries, whose length
// is a byte; the CM messages limit it further.
const maxPrivateData = 255

// PortSpace is the port space of an identifier (enum rdma_ucm_port_space).
type PortSpace uint16

// Port spaces. The kernel gives an identifier of PortSpaceTCP the RC queue
// pair type, of PortSpaceUDP and PortSpaceIPoIB the UD type, and of
// PortSpaceIB the type CreateID names.
const (
	PortSpaceIPoIB PortSpace = 0x0002
	PortSpaceTCP   PortSpace = 0x0106
	PortSpaceUDP   PortSpace = 0x0111
	PortSpaceIB    PortSpace = 0x013F
)

// QPType is a queue pair type (enum ibv_qp_type).
type QPType uint8

// Queue pair types.
const (
	QPTypeRC QPType = 2
	QPTypeUC QPType = 3
	QPTypeUD QPType = 4
)

// ConnParam is the connection form of the parameters of Connect, Accept and
// the events (struct ucma_abi_conn_param).
type ConnParam struct {
	PrivateData        []byte // at most 255 bytes
	QPNum              uint32
	SRQ                bool
	ResponderResources uint8
	InitiatorDepth     uint8
	FlowControl        uint8
	RetryCount         uint8
	RNRRetryCount      uint8
}

// put writes p into the parameters of a request and marks them valid.
func (p *ConnParam) put(dst *rdmaabi.UcmaAbiConnParam) error {
	if len(p.PrivateData) > maxPrivateData {
		return errPrivateData(len(p.PrivateData))
	}
	*dst = rdmaabi.UcmaAbiConnParam{
		Qp_num:              p.QPNum,
		Private_data_len:    uint8(len(p.PrivateData)),
		Responder_resources: p.ResponderResources,
		Initiator_depth:     p.InitiatorDepth,
		Flow_control:        p.FlowControl,
		Retry_count:         p.RetryCount,
		Rnr_retry_count:     p.RNRRetryCount,
		Valid:               1,
	}
	if p.SRQ {
		dst.Srq = 1
	}
	copy(dst.Private_data[:], p.PrivateData)
	return nil
}

func errPrivateData(n int) error {
	return fmt.Errorf("ucma: %d bytes of private data, at most %d fit: %w", n, maxPrivateData, syscall.EINVAL)
}

// ECE is the vendor option exchange of Connect, Accept and the connection
// events (struct ucma_abi_ece); the zero ECE offers none.
type ECE struct {
	VendorID uint32
	AttrMod  uint32
}

func (e ECE) abi() rdmaabi.UcmaAbiEce {
	return rdmaabi.UcmaAbiEce{Vendor_id: e.VendorID, Attr_mod: e.AttrMod}
}

// CreateID creates an identifier in the port space ps and returns its
// number; qpType counts for PortSpaceIB only. The events of the identifier
// carry uid.
func (c *Channel) CreateID(uid uint64, ps PortSpace, qpType QPType) (uint32, error) {
	cmd := rdmaabi.UcmaAbiCreateId{Uid: uid, Ps: uint16(ps), Qp_type: uint8(qpType)}
	var resp rdmaabi.UcmaAbiCreateIdResp
	if err := call(c, cmdCreateID, &cmd, &cmd.Response, &resp); err != nil {
		return 0, err
	}
	return resp.Id, nil
}

// DestroyID destroys an identifier with its queued events and returns how
// many of its events the channel has handed out.
func (c *Channel) DestroyID(id uint32) (eventsReported uint32, err error) {
	cmd := rdmaabi.UcmaAbiDestroyId{Id: id}
	var resp rdmaabi.UcmaAbiDestroyIdResp
	if err := call(c, cmdDestroyID, &cmd, &cmd.Response, &resp); err != nil {
		return 0, err
	}
	return resp.Events_reported, nil
}

// Bind binds an identifier to a local address, which picks the device
// unless it is unspecified; port 0 takes a free port.
func (c *Channel) Bind(id uint32, addr netip.AddrPort) error {
	ss, size, err := sockaddr(addr)
	if err != nil {
		return err
	}
	cmd := rdmaabi.UcmaAbiBind{Id: id, Addr_size: size, Addr: ss}
	return command(c, cmdBind, &cmd, 0)
}

// BindIB binds an identifier to a local InfiniBand address as Bind does to
// an IP address: the GID picks the device unless it is :: or ::1, and the
// service ID picks the port space and the port.
func (c *Channel) BindIB(id uint32, addr IBAddr) error {
	ss, size, err := addr.sockaddr()
	if err != nil {
		return err
	}
	cmd := rdmaabi.UcmaAbiBind{Id: id, Addr_size: size, Addr: ss}
	return command(c, cmdBind, &cmd, 0)
}

// ResolveAddr starts resolving the address dst from src, or from an address
// the kernel picks when src is the zero AddrPort. It ends in
// EventAddrResolved or EventAddrError, after timeout at the latest.
func (c *Channel) ResolveAddr(id uint32, src, dst netip.AddrPort, timeout time.Duration) error {
	cmd := rdmaabi.UcmaAbiResolveAddr{Id: id, Timeout_ms: millis(timeout)}
	var err error
	if cmd.Dst_addr, cmd.Dst_size, err = sockaddr(dst); err != nil {
		return err
	}
	if src.IsValid() {
		cmd.Src_addr, cmd.Src_size, _ = sockaddr(src)
	}
	return command(c, cmdResolveAddr, &cmd, 0)
}

// ResolveIBAddr starts resolving the InfiniBand address dst as ResolveAddr
// does an IP address, from src or, when src is the zero IBAddr, from an
// address the kernel picks.
func (c *Channel) ResolveIBAddr(id uint32, src, dst IBAddr, timeout time.Duration) error {
	cmd := rdmaabi.UcmaAbiResolveAddr{Id: id, Timeout_ms: millis(timeout)}
	var err error
	if cmd.Dst_addr, cmd.Dst_size, err = dst.sockaddr(); err != nil {
		return err
	}
	if src.GID.IsValid() {
		cmd.Src_addr, cmd.Src_size, _ = src.sockaddr()
	}
	return command(c, cmdResolveAddr, &cmd, 0)
}

// ResolveRoute starts resolving the route of an identifier whose address is
// resolved. It ends in EventRouteResolved or EventRouteError, after timeout
// at the latest.
func (c *Channel) ResolveRoute(id uint32, timeout time.Duration) error {
	cmd := rdmaabi.UcmaAbiResolveRoute{Id: id, Timeout_ms: millis(timeout)}
	return command(c, cmdResolveRoute, &cmd, 0)
}

// millis is d in whole milliseconds, rounded up, within the kernel's int.
func millis(d time.Duration) uint32 {
	if d <= 0 {
		return 0
	}
	ms := d / time.Millisecond
	if d%time.Millisecond != 0 {
		ms++
	}
	return uint32(min(ms, math.MaxInt32))
}

// AddrInfo is what QueryAddr learns of an identifier (struct
// ucma_abi_query_addr_resp).
type AddrInfo struct {
	Src, Dst netip.AddrPort // the local and the peer's address, zero while unknown
	// SrcIB and DstIB are the local and the peer's address in place of Src
	// and Dst when these are InfiniBand addresses (AF_IB), zero otherwise.
	SrcIB, DstIB IBAddr
	// The device of the identifier: its node GUID, 0 while the identifier
	// has no device, which leaves the rest meaningless; the port; the
	// partition key; the kernel's index of the device, -1 from a kernel
	// that does not report it.
	NodeGUID uint64
	PortNum  uint8
	PKey     uint16
	DevIndex int
}

// QueryAddr queries the addresses and the device of an identifier.
func (c *Channel) QueryAddr(id uint32) (AddrInfo, error) { return c.queryAddrs(id, queryAddr) }

// QueryGID queries the addresses of an identifier in their GID form, as
// InfiniBand addresses in SrcIB and DstIB, with the device as QueryAddr
// reports it. The kernel answers an identifier of AF_IB with its addresses,
// and one of IP with the GIDs of its route, the partition key of its port
// and the service ID of its port space and port.
func (c *Channel) QueryGID(id uint32) (AddrInfo, error) { return c.queryAddrs(id, queryGID) }

// queryAddrs sends a query whose response is a struct
// ucma_abi_query_addr_resp and reads the response.
func (c *Channel) queryAddrs(id, option uint32) (AddrInfo, error) {
	cmd := rdmaabi.UcmaAbiQuery{Id: id, Option: option}
	// A kernel that does not report the index leaves the field as it is.
	resp := rdmaabi.UcmaAbiQueryAddrResp{Ibdev_index: math.MaxUint32}
	if err := call(c, cmdQuery, &cmd, &cmd.Response, &resp); err != nil {
		return AddrInfo{}, err
	}
	info := AddrInfo{
		Src:      addrPort(&resp.Src_addr),
		Dst:      addrPort(&resp.Dst_addr),
		SrcIB:    ibAddr(&resp.Src_addr),
		DstIB:    ibAddr(&resp.Dst_addr),
		NodeGUID: binary.BigEndian.Uint64(resp.Node_guid[:]),
		PortNum:  resp.Port_num,
		PKey:     binary.BigEndian.Uint16(resp.Pkey[:]),
		DevIndex: -1,
	}
	if resp.Ibdev_index != math.MaxUint32 {
		info.DevIndex = int(resp.Ibdev_index)
	}
	return info, nil
}

// Listen makes a bound identifier listen for connection requests. backlog
// bounds the requests not yet fetched; 0, a negative backlog or one above
// the kernel's maximum (1024 by default) means that maximum.
func (c *Channel) Listen(id uint32, backlog int) error {
	cmd := rdmaabi.UcmaAbiListen{Id: id, Backlog: uint32(backlog)}
	return command(c, cmdListen, &cmd, 0)
}

// Connect sends a connection request from an identifier whose route is
// resolved. It ends in EventConnectResponse, EventEstablished on iWARP, or
// in EventRejected, EventUnreachable or EventConnectError.
func (c *Channel) Connect(id uint32, param ConnParam, ece ECE) error {
	cmd := rdmaabi.UcmaAbiConnect{Id: id, Ece: ece.abi()}
	if err := param.put(&cmd.Conn_param); err != nil {
		return err
	}
	return command(c, cmdConnect, &cmd, 0)
}

// Accept accepts the connection request of an EventConnectRequest on the
// identifier the event carries; the events of that identifier carry uid
// from then on. A nil param sends the form without parameters, which the
// active side sends once its queue pair is ready to send, and for which
// the kernel ignores uid.
func (c *Channel) Accept(id uint32, uid uint64, param *ConnParam, ece ECE) error {
	cmd := rdmaabi.UcmaAbiAccept{Id: id, Uid: uid, Ece: ece.abi()}
	if param != nil {
		if err := param.put(&cmd.Conn_param); err != nil {
			return err
		}
	}
	return command(c, cmdAccept, &cmd, 0)
}

// Reject rejects the connection request of an identifier with a reason and
// private data, which the peer's EventRejected carries; reason 0 is the
// consumer-defined reason, 28.
func (c *Channel) Reject(id uint32, reason uint8, privateData []byte) error {
	if len(privateData) > maxPrivateData {
		return errPrivateData(len(privateData))
	}
	cmd := rdmaabi.UcmaAbiReject{Id: id, Reason: reason, Private_data_len: uint8(len(privateData))}
	copy(cmd.Private_data[:], privateData)
	return command(c, cmdReject, &cmd, 0)
}

// Disconnect disconnects the connection of an identifier, which ends in
// EventDisconnected.
func (c *Channel) Disconnect(id uint32) error {
	cmd := rdmaabi.UcmaAbiDisconnect{Id: id}
	return command(c, cmdDisconnect, &cmd, 0)
}

// InitQPAttr returns the attributes the kernel has for moving the queue
// pair of an identifier to state, an ibv_qp_state value (1 INIT, 2 RTR,
// 3 RTS, ...), with the mask of the attributes it set in Qp_attr_mask.
func (c *Channel) InitQPAttr(id uint32, state uint32) (rdmaabi.IbUverbsQpAttr, error) {
	cmd := rdmaabi.UcmaAbiInitQpAttr{Id: id, Qp_state: state}
	var resp rdmaabi.IbUverbsQpAttr
	if err := call(c, cmdInitQPAttr, &cmd, &cmd.Response, &resp); err != nil {
		return rdmaabi.IbUverbsQpAttr{}, err
	}
	return resp, nil
}

// MigrateID moves an identifier with its queued events from the channel
// from to c and returns how many of its events from has handed out. The
// kernel queues the moved events on c without a wake-up: a GetEvent already
// waiting on c is not woken by them, while a wait started after MigrateID
// returns sees them.
func (c *Channel) MigrateID(from *Channel, id uint32) (eventsReported uint32, err error) {
	var resp rdmaabi.UcmaAbiMigrateResp
	var cmdErr error
	if err := from.raw.Control(func(fd uintptr) {
		cmd := rdmaabi.UcmaAbiMigrateId{Id: id, Fd: uint32(fd)}
		cmdErr = call(c, cmdMigrateID, &cmd, &cmd.Response, &resp)
	}); err != nil {
		return 0, os.ErrClosed
	}
	if cmdErr != nil {
		return 0, cmdErr
	}
	return resp.Events_reported, nil
}
