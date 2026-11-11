package rsproto

import "encoding/binary"

// Sizes and limits of the protocol (rsocket.c lines 65-72).
const (
	OLAPStartSize = 2048   // RS_OLAP_START_SIZE: the first message of a send, each next one doubling up to MaxTransfer
	MaxTransfer   = 65536  // RS_MAX_TRANSFER: the largest data message
	SndLowat      = 2048   // RS_SNDLOWAT: the free send buffer a sender waits for
	QPMinSize     = 16     // RS_QP_MIN_SIZE: the least send or receive queue depth
	QPMaxSize     = 0xFFFE // RS_QP_MAX_SIZE: the greatest send or receive queue depth
	QPCtrlSize    = 4      // RS_QP_CTRL_SIZE: grant, credit and control messages in flight at once
	SGLSize       = 2      // RS_SGL_SIZE: the entries of a grant table
)

// Stock defaults of rsockets (rsocket.c lines 139-144), which the files
// under /etc/rdma/rsocket override for a C process.
const (
	DefaultSendBufferSize    = 1 << 17 // def_wmem
	DefaultReceiveBufferSize = 1 << 17 // def_mem
	DefaultSendQueueSize     = 384     // def_sqsize
	DefaultReceiveQueueSize  = 384     // def_rqsize
	DefaultMaxInline         = 64      // def_inline
)

// Op is the operation in the top 3 bits of a message word (rsocket.c lines
// 163-171). rs_poll_cq takes the reserved values 1, 3 and 5 for OpData.
type Op uint8

const (
	OpData     Op = 0 // RS_OP_DATA: bytes written into the current grant; the data is their count
	OpWrite    Op = 2 // RS_OP_WRITE: a direct write, which never travels; ignored when received
	OpSGL      Op = 4 // RS_OP_SGL: a grant or bare credits; the data is the new credit limit
	OpIOMapSGL Op = 6 // RS_OP_IOMAP_SGL: a direct-I/O mapping; ignored when received
	OpCtrl     Op = 7 // RS_OP_CTRL: a control message; the data is a Ctrl code
)

// Control codes, the data of an OpCtrl message (rsocket.c lines 186-189).
const (
	CtrlDisconnect = 0 // RS_CTRL_DISCONNECT: the connection ends
	CtrlKeepalive  = 1 // RS_CTRL_KEEPALIVE: stock rsockets sends it without immediate data, so it never arrives; ignored when received
	CtrlShutdown   = 2 // RS_CTRL_SHUTDOWN: the peer writes no more
)

// Msg is a message word, the immediate data of every message: an Op in the
// top 3 bits and 29 bits of data (rsocket.c lines 173-175). It travels in
// network byte order, as verbs sends immediate data.
type Msg uint32

// MakeMsg mirrors rs_msg_set, which does not mask data to 29 bits.
func MakeMsg(op Op, data uint32) Msg { return Msg(op)<<29 | Msg(data) }

// Op mirrors rs_msg_op.
func (m Msg) Op() Op { return Op(m >> 29) }

// Data mirrors rs_msg_data.
func (m Msg) Data() uint32 { return uint32(m) & 0x1FFFFFFF }

// SGE mirrors struct rs_sge (rsocket.c line 209): memory of one side that
// the other may write, in a grant entry or the connection data.
type SGE struct {
	Addr   uint64
	Key    uint32
	Length uint32
}

// SGESize is sizeof(struct rs_sge), the size of a grant entry.
const SGESize = 16

// PutSGE writes s into b[:SGESize] in the given byte order.
func PutSGE(b []byte, order binary.ByteOrder, s SGE) {
	b = b[:SGESize]
	order.PutUint64(b[0:8], s.Addr)
	order.PutUint32(b[8:12], s.Key)
	order.PutUint32(b[12:16], s.Length)
}

// GetSGE reads an SGE from b[:SGESize] in the given byte order.
func GetSGE(b []byte, order binary.ByteOrder) SGE {
	b = b[:SGESize]
	return SGE{Addr: order.Uint64(b[0:8]), Key: order.Uint32(b[8:12]), Length: order.Uint32(b[12:16])}
}
