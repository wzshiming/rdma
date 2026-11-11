package rsproto_test

import (
	"encoding/binary"
	"testing"

	"github.com/wzshiming/rdma/internal/rsproto"
)

func TestConstantsMatchRsocketC(t *testing.T) {
	tests := []struct {
		c         string // the C definition, by line of rdma-core v66.0 librdmacm/rsocket.c
		got, want int
	}{
		{"RS_OLAP_START_SIZE (65)", rsproto.OLAPStartSize, 2048},
		{"RS_MAX_TRANSFER (66)", rsproto.MaxTransfer, 65536},
		{"RS_SNDLOWAT (67)", rsproto.SndLowat, 2048},
		{"RS_QP_MIN_SIZE (68)", rsproto.QPMinSize, 16},
		{"RS_QP_MAX_SIZE (69)", rsproto.QPMaxSize, 0xFFFE},
		{"RS_QP_CTRL_SIZE (70)", rsproto.QPCtrlSize, 4},
		{"RS_SGL_SIZE (72)", rsproto.SGLSize, 2},
		{"def_inline (140)", rsproto.DefaultMaxInline, 64},
		{"def_sqsize (141)", rsproto.DefaultSendQueueSize, 384},
		{"def_rqsize (142)", rsproto.DefaultReceiveQueueSize, 384},
		{"def_mem (143)", rsproto.DefaultReceiveBufferSize, 1 << 17},
		{"def_wmem (144)", rsproto.DefaultSendBufferSize, 1 << 17},
	}
	for _, tt := range tests {
		if tt.got != tt.want {
			t.Errorf("%s = %d, want %d", tt.c, tt.got, tt.want)
		}
	}
}

// The wire bytes are the immediate data as rs_post_msg and rs_post_write_msg
// send it, htobe32(rs_msg_set(op, data)), and as rs_poll_cq reads it back
// with be32toh, rs_msg_op and rs_msg_data.
func TestMsgWireFormat(t *testing.T) {
	tests := []struct {
		c    string // the C function that builds the word
		op   rsproto.Op
		data uint32
		wire [4]byte
	}{
		{"rs_write_data of 1 byte", rsproto.OpData, 1, [4]byte{0x00, 0x00, 0x00, 0x01}},
		{"rs_write_data of RS_MAX_TRANSFER bytes", rsproto.OpData, rsproto.MaxTransfer, [4]byte{0x00, 0x01, 0x00, 0x00}},
		{"rs_msg_data at its widest", rsproto.OpData, 0x1FFFFFFF, [4]byte{0x1f, 0xff, 0xff, 0xff}},
		{"rs_write_direct, a wr_id that never travels", rsproto.OpWrite, 4096, [4]byte{0x40, 0x00, 0x10, 0x00}},
		{"rs_send_credits with rseq_no 0 and rq_size 384", rsproto.OpSGL, 384, [4]byte{0x80, 0x00, 0x01, 0x80}},
		{"rs_send_credits with rseq_no 0xffff and rq_size 0xfffe, past 16 bits", rsproto.OpSGL, 0xFFFF + 0xFFFE, [4]byte{0x80, 0x01, 0xff, 0xfd}},
		{"rs_write_iomap of index 3", rsproto.OpIOMapSGL, 3, [4]byte{0xc0, 0x00, 0x00, 0x03}},
		{"rshutdown(SHUT_RDWR)", rsproto.OpCtrl, rsproto.CtrlDisconnect, [4]byte{0xe0, 0x00, 0x00, 0x00}},
		{"tcp_svc_send_keepalive, a wr_id that never travels", rsproto.OpCtrl, rsproto.CtrlKeepalive, [4]byte{0xe0, 0x00, 0x00, 0x01}},
		{"rshutdown(SHUT_WR)", rsproto.OpCtrl, rsproto.CtrlShutdown, [4]byte{0xe0, 0x00, 0x00, 0x02}},
	}
	for _, tt := range tests {
		t.Run(tt.c, func(t *testing.T) {
			var got [4]byte
			binary.BigEndian.PutUint32(got[:], uint32(rsproto.MakeMsg(tt.op, tt.data)))
			if got != tt.wire {
				t.Errorf("MakeMsg(%d, %#x) = % x on the wire, want % x", tt.op, tt.data, got, tt.wire)
			}
			m := rsproto.Msg(binary.BigEndian.Uint32(tt.wire[:]))
			if m.Op() != tt.op || m.Data() != tt.data {
				t.Errorf("% x reads as op %d, data %#x; want op %d, data %#x", tt.wire, m.Op(), m.Data(), tt.op, tt.data)
			}
		})
	}
}

// A grant entry is a struct rs_sge, an address, a key and a length, which
// rs_send_credits writes into the peer's grant table field by field in one
// byte order, and which rs_can_send and rs_write_data read in place.
func TestSGEWireFormat(t *testing.T) {
	grant := rsproto.SGE{Addr: 0x00007f1122334400, Key: 0x55667788, Length: 0x00010000}
	tests := []struct {
		c     string // the C function and case that writes the entry
		order binary.ByteOrder
		wire  [rsproto.SGESize]byte
	}{
		{
			"rs_send_credits on a little-endian host for a little-endian peer, in host order",
			binary.LittleEndian,
			[rsproto.SGESize]byte{0x00, 0x44, 0x33, 0x22, 0x11, 0x7f, 0x00, 0x00, 0x88, 0x77, 0x66, 0x55, 0x00, 0x00, 0x01, 0x00},
		},
		{
			"rs_send_credits on a little-endian host for a big-endian peer, swapped by bswap_64 and bswap_32",
			binary.BigEndian,
			[rsproto.SGESize]byte{0x00, 0x00, 0x7f, 0x11, 0x22, 0x33, 0x44, 0x00, 0x55, 0x66, 0x77, 0x88, 0x00, 0x01, 0x00, 0x00},
		},
	}
	for _, tt := range tests {
		t.Run(tt.c, func(t *testing.T) {
			var got [rsproto.SGESize]byte
			rsproto.PutSGE(got[:], tt.order, grant)
			if got != tt.wire {
				t.Errorf("PutSGE(%v, %+v) = % x, want % x", tt.order, grant, got, tt.wire)
			}
			if back := rsproto.GetSGE(tt.wire[:], tt.order); back != grant {
				t.Errorf("GetSGE(% x, %v) = %+v, want %+v", tt.wire, tt.order, back, grant)
			}
		})
	}
}
