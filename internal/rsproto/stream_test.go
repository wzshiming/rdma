package rsproto_test

import (
	"slices"
	"testing"

	"github.com/wzshiming/rdma/internal/rsproto"
)

// post is one call a stream made of its link.
type post struct {
	op   string // the C function the call mirrors
	id   uint64
	sgl  []rsproto.Segment
	msg  rsproto.Msg
	addr uint64
	key  uint32
}

// recorder is a link that records what a stream posts.
type recorder struct {
	posts []post
}

func (r *recorder) PostWriteMsg(id uint64, sgl []rsproto.Segment, msg rsproto.Msg, addr uint64, key uint32) error {
	r.posts = append(r.posts, post{"rs_post_write_msg", id, slices.Clone(sgl), msg, addr, key})
	return nil
}

func (r *recorder) PostMsg(id uint64, msg rsproto.Msg) error {
	r.posts = append(r.posts, post{op: "rs_post_msg", id: id, msg: msg})
	return nil
}

func (r *recorder) PostRecv(id uint64) error {
	r.posts = append(r.posts, post{op: "rs_post_recv", id: id})
	return nil
}

// testConfig describes local memory as an engine registers it, at addresses
// and under keys that tell the blocks apart.
func testConfig() rsproto.Config {
	return rsproto.Config{
		SendBuffer:       rsproto.Region{Buf: make([]byte, 1<<16), Addr: 0x00007e0000000000, Key: 0x5e4d},
		ReceiveBuffer:    rsproto.Region{Buf: make([]byte, 1<<16), Addr: 0x00007e1000000000, Key: 0x4ecf},
		GrantTable:       rsproto.Region{Buf: make([]byte, rsproto.SGLSize*rsproto.SGESize), Addr: 0x00007e2000000000, Key: 0x6a17},
		SendQueueSize:    rsproto.DefaultSendQueueSize,
		ReceiveQueueSize: rsproto.DefaultReceiveQueueSize,
	}
}

func TestStreamConnData(t *testing.T) {
	c := testConfig()
	little := rsproto.ConnData{
		Credits:    rsproto.DefaultReceiveQueueSize,
		GrantTable: rsproto.SGE{Addr: c.GrantTable.Addr, Key: c.GrantTable.Key, Length: rsproto.SGLSize},
		FirstGrant: rsproto.SGE{Addr: c.ReceiveBuffer.Addr, Key: c.ReceiveBuffer.Key, Length: 1 << 15},
	}
	big := little
	big.BigEndian = true

	tests := []struct {
		c         string // the C function and case that formats the data
		bigEndian bool
		want      rsproto.ConnData
	}{
		{"rs_format_conn_data on a little-endian host", false, little},
		{"rs_format_conn_data on a big-endian host", true, big},
	}
	for _, tt := range tests {
		t.Run(tt.c, func(t *testing.T) {
			c.BigEndian = tt.bigEndian
			if got := rsproto.NewStream(&recorder{}, c).ConnData(); got != tt.want {
				t.Errorf("ConnData() = %+v, want %+v", got, tt.want)
			}
		})
	}
}
