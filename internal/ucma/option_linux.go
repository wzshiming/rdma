//go:build linux && !android && (amd64 || arm64 || loong64 || ppc64le || riscv64 || s390x)

package ucma

import (
	"encoding/binary"
	"runtime"

	"github.com/wzshiming/rdma/internal/rdmaabi"
)

// cmdSetOption is the command of SetOption, numbered as in
// rdma/rdma_cma_abi.h.
const cmdSetOption = 14

// OptionLevel is the level of an option (RDMA_OPTION_ID, RDMA_OPTION_IB).
type OptionLevel uint32

// Option levels.
const (
	OptionLevelID OptionLevel = 0 // options of the identifier
	OptionLevelIB OptionLevel = 1 // InfiniBand options
)

// OptionName names an option of a level.
type OptionName uint32

// The options of OptionLevelID, with the C type of their values, and of
// OptionLevelIB. The kernel refuses another name with ENOSYS and a value of
// another size with EINVAL.
const (
	OptionIDTOS        OptionName = 0 // uint8_t: the type of service
	OptionIDReuseAddr  OptionName = 1 // int: like SO_REUSEADDR
	OptionIDAFOnly     OptionName = 2 // int: like IPV6_V6ONLY
	OptionIDAckTimeout OptionName = 3 // uint8_t: the local ACK timeout
	OptionIBPath       OptionName = 1 // struct ibv_path_data[]: the path records
)

// SetOption sets an option of an identifier to the bytes of value, which
// the kernel copies during the call.
func (c *Channel) SetOption(id uint32, level OptionLevel, name OptionName, value []byte) error {
	cmd := rdmaabi.UcmaAbiSetOption{Id: id, Level: uint32(level), Optname: uint32(name), Optlen: uint32(len(value))}
	var pin runtime.Pinner
	defer pin.Unpin()
	if len(value) > 0 {
		cmd.Optval = pinned(&pin, &value[0])
	}
	return command(c, cmdSetOption, &cmd, 0)
}

// SetTOS sets the type of service of an identifier's traffic, which the
// routes it resolves afterwards carry.
func (c *Channel) SetTOS(id uint32, tos uint8) error {
	return c.SetOption(id, OptionLevelID, OptionIDTOS, []byte{tos})
}

// SetReuseAddr sets whether an identifier may bind to an address in use;
// the kernel takes it before Bind, and reuse also later but not once the
// identifier listens.
func (c *Channel) SetReuseAddr(id uint32, reuse bool) error {
	return c.SetOption(id, OptionLevelID, OptionIDReuseAddr, cInt(reuse))
}

// SetAFOnly sets whether an identifier bound to an IPv6 address takes IPv6
// only; the kernel takes it before the identifier resolves or listens.
func (c *Channel) SetAFOnly(id uint32, afOnly bool) error {
	return c.SetOption(id, OptionLevelID, OptionIDAFOnly, cInt(afOnly))
}

// SetAckTimeout sets the local ACK timeout of the queue pair of an RC
// identifier, 4.096 µs × 2^timeout, which InitQPAttr reports for RTS from
// then on.
func (c *Channel) SetAckTimeout(id uint32, timeout uint8) error {
	return c.SetOption(id, OptionLevelID, OptionIDAckTimeout, []byte{timeout})
}

// cInt is the memory of a C int that holds b as 1 or 0.
func cInt(b bool) []byte {
	var v uint32
	if b {
		v = 1
	}
	return binary.NativeEndian.AppendUint32(nil, v)
}
