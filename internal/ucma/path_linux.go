//go:build linux && !android && (amd64 || arm64 || loong64 || ppc64le || riscv64 || s390x)

package ucma

import (
	"encoding/binary"
	"net/netip"
	"unsafe"

	"github.com/wzshiming/rdma/internal/rdmaabi"
)

// PathFlags are the flags of a path record (IBV_PATH_FLAG_*).
type PathFlags uint32

// Path record flags.
const (
	PathGMP            PathFlags = 1 << 0 // the path of the CM's management packets
	PathPrimary        PathFlags = 1 << 1
	PathAlternate      PathFlags = 1 << 2
	PathOutbound       PathFlags = 1 << 3
	PathInbound        PathFlags = 1 << 4
	PathInboundReverse PathFlags = 1 << 5
	PathBidirectional            = PathOutbound | PathInboundReverse
)

// PathRecord is an InfiniBand path record, struct ibv_path_record with its
// bit fields apart; a field is cut to its bits when encoded.
type PathRecord struct {
	ServiceID  uint64
	DGID, SGID netip.Addr // as IBAddr.GID
	DLID, SLID uint16
	RawTraffic bool
	FlowLabel  uint32 // 20 bits
	HopLimit   uint8
	// TrafficClass is the traffic class, the type of service on RoCE.
	TrafficClass uint8
	Reversible   bool
	NumPath      uint8 // 7 bits
	PKey         uint16
	QoSClass     uint16 // 12 bits
	SL           uint8  // 4 bits
	// MTU, Rate and PacketLifeTime are 6-bit values (enum ibv_mtu, enum
	// ibv_rate and 4.096 µs × 2^PacketLifeTime), each with a 2-bit
	// selector: 0 greater than, 1 less than, 2 exactly, 3 the best.
	MTUSelector, MTU                       uint8
	RateSelector, Rate                     uint8
	PacketLifeTimeSelector, PacketLifeTime uint8
	Preference                             uint8
}

// PathData is a path record with its flags (struct ibv_path_data), the
// form of the path query and of the path option.
type PathData struct {
	Flags PathFlags
	Path  PathRecord
}

// abi returns p in the form of struct ibv_path_data.
func (p PathData) abi() rdmaabi.IbvPathData {
	r := &p.Path
	d := rdmaabi.IbvPathData{Flags: uint32(p.Flags)}
	w := &d.Path
	binary.BigEndian.PutUint64(w.Service_id[:], r.ServiceID)
	w.Dgid = r.DGID.As16()
	w.Sgid = r.SGID.As16()
	binary.BigEndian.PutUint16(w.Dlid[:], r.DLID)
	binary.BigEndian.PutUint16(w.Slid[:], r.SLID)
	binary.BigEndian.PutUint32(w.Flowlabel_hoplimit[:], uint32(bit(r.RawTraffic))<<31|r.FlowLabel&0xfffff<<8|uint32(r.HopLimit))
	w.Tclass = r.TrafficClass
	w.Reversible_numpath = bit(r.Reversible)<<7 | r.NumPath&0x7f
	binary.BigEndian.PutUint16(w.Pkey[:], r.PKey)
	binary.BigEndian.PutUint16(w.Qosclass_sl[:], r.QoSClass&0xfff<<4|uint16(r.SL&0xf))
	w.Mtu = r.MTUSelector<<6 | r.MTU&0x3f
	w.Rate = r.RateSelector<<6 | r.Rate&0x3f
	w.Packetlifetime = r.PacketLifeTimeSelector<<6 | r.PacketLifeTime&0x3f
	w.Preference = r.Preference
	return d
}

func bit(b bool) uint8 {
	if b {
		return 1
	}
	return 0
}

// pathData reads a struct ibv_path_data.
func pathData(d *rdmaabi.IbvPathData) PathData {
	w := &d.Path
	flowHop := binary.BigEndian.Uint32(w.Flowlabel_hoplimit[:])
	qosSL := binary.BigEndian.Uint16(w.Qosclass_sl[:])
	return PathData{
		Flags: PathFlags(d.Flags),
		Path: PathRecord{
			ServiceID:              binary.BigEndian.Uint64(w.Service_id[:]),
			DGID:                   netip.AddrFrom16(w.Dgid),
			SGID:                   netip.AddrFrom16(w.Sgid),
			DLID:                   binary.BigEndian.Uint16(w.Dlid[:]),
			SLID:                   binary.BigEndian.Uint16(w.Slid[:]),
			RawTraffic:             flowHop>>31 != 0,
			FlowLabel:              flowHop >> 8 & 0xfffff,
			HopLimit:               uint8(flowHop),
			TrafficClass:           w.Tclass,
			Reversible:             w.Reversible_numpath>>7 != 0,
			NumPath:                w.Reversible_numpath & 0x7f,
			PKey:                   binary.BigEndian.Uint16(w.Pkey[:]),
			QoSClass:               qosSL >> 4,
			SL:                     uint8(qosSL & 0xf),
			MTUSelector:            w.Mtu >> 6,
			MTU:                    w.Mtu & 0x3f,
			RateSelector:           w.Rate >> 6,
			Rate:                   w.Rate & 0x3f,
			PacketLifeTimeSelector: w.Packetlifetime >> 6,
			PacketLifeTime:         w.Packetlifetime & 0x3f,
			Preference:             w.Preference,
		},
	}
}

// pathSlots is how many path records the response to a path query has room
// for, as librdmacm's has; a route has two at most, the primary path and the
// alternate.
const pathSlots = 6

// QueryPath queries the path records of an identifier's route, none before
// the route is resolved. The kernel flags each PathGMP, PathPrimary and
// PathBidirectional.
func (c *Channel) QueryPath(id uint32) ([]PathData, error) {
	cmd := rdmaabi.UcmaAbiQuery{Id: id, Option: queryPath}
	var resp struct {
		rdmaabi.UcmaAbiQueryPathResp
		Path_data [pathSlots]rdmaabi.IbvPathData
	}
	if err := call(c, cmdQuery, &cmd, &cmd.Response, &resp); err != nil {
		return nil, err
	}
	var paths []PathData
	for i := range min(int(resp.Num_paths), pathSlots) {
		paths = append(paths, pathData(&resp.Path_data[i]))
	}
	return paths, nil
}

// SetIBPath sets the route of an identifier whose address is resolved to
// the path records paths in place of ResolveRoute, as librdmacm sets the
// route rdma_getaddrinfo finds (RDMA_OPTION_IB_PATH). The kernel takes the
// first record flagged PathGMP|PathPrimary|PathBidirectional, fails with
// EINVAL without one, and queues EventRouteResolved itself.
func (c *Channel) SetIBPath(id uint32, paths []PathData) error {
	var value []byte
	for _, p := range paths {
		d := p.abi()
		value = append(value, unsafe.Slice((*byte)(unsafe.Pointer(&d)), unsafe.Sizeof(d))...)
	}
	return c.SetOption(id, OptionLevelIB, OptionIBPath, value)
}
