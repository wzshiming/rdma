package rsproto_test

import (
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
