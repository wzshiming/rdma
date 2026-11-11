package rsproto

import (
	"encoding/binary"
	"errors"
	"fmt"
	"io"
)

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

// ConnData mirrors struct rs_conn_data (rsocket.c line 233), which each side
// puts in the private data of the connection request and reply, big-endian.
// Direct-I/O mappings are never advertised.
type ConnData struct {
	BigEndian  bool   // RS_CONN_FLAG_NET: the sender's host is big-endian
	Credits    uint16 // the sender's receive queue depth
	GrantTable SGE    // target_sgl: the sender's grant table, Length counting entries
	FirstGrant SGE    // data_buf: the first half of the sender's receive buffer
}

// ConnDataSize is sizeof(struct rs_conn_data).
const ConnDataSize = 40

const (
	connFlagNet   = 1 << 0 // RS_CONN_FLAG_NET
	connFlagIOMap = 1 << 1 // RS_CONN_FLAG_IOMAP
)

// ByteOrder is the byte order of the sender's host, and so of the grant
// entries written for it: rs_send_credits writes them in its own host order,
// swapped when the hosts differ (RS_OPT_SWAP_SGL).
func (c ConnData) ByteOrder() binary.ByteOrder {
	if c.BigEndian {
		return binary.BigEndian
	}
	return binary.LittleEndian
}

// Encode mirrors rs_format_conn_data, which sets RS_CONN_FLAG_IOMAP even for
// an iomap of 0 entries, the only size Encode writes.
func (c ConnData) Encode() [ConnDataSize]byte {
	var b [ConnDataSize]byte
	b[0] = 1
	b[1] = connFlagIOMap
	if c.BigEndian {
		b[1] |= connFlagNet
	}
	binary.BigEndian.PutUint16(b[2:4], c.Credits)
	PutSGE(b[8:24], binary.BigEndian, c.GrantTable)
	PutSGE(b[24:40], binary.BigEndian, c.FirstGrant)
	return b
}

// ErrVersion reports connection data of a version other than 1, which
// rs_accept rejects and rs_do_connect fails with ENOTSUP.
var ErrVersion = errors.New("rsproto: unsupported connection data version")

// DecodeConnData mirrors rs_save_conn_data. Fewer than ConnDataSize bytes
// yield an error wrapping io.ErrUnexpectedEOF; the bytes after them, the
// CM's zero padding of the private data, are ignored, as is the peer's iomap.
func DecodeConnData(b []byte) (ConnData, error) {
	if len(b) < ConnDataSize {
		return ConnData{}, fmt.Errorf("rsproto: connection data of %d bytes, want %d: %w", len(b), ConnDataSize, io.ErrUnexpectedEOF)
	}
	if b[0] != 1 {
		return ConnData{}, fmt.Errorf("%w %d", ErrVersion, b[0])
	}
	return ConnData{
		BigEndian:  b[1]&connFlagNet != 0,
		Credits:    binary.BigEndian.Uint16(b[2:4]),
		GrantTable: GetSGE(b[8:24], binary.BigEndian),
		FirstGrant: GetSGE(b[24:40], binary.BigEndian),
	}, nil
}
