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

// testConfig describes local memory of the stock sizes as an engine registers
// it, at addresses and under keys that tell the blocks apart.
func testConfig() rsproto.Config {
	return rsproto.Config{
		SendBuffer:       rsproto.Region{Buf: make([]byte, rsproto.DefaultSendBufferSize), Addr: 0x00007e0000000000, Key: 0x5e4d},
		ReceiveBuffer:    rsproto.Region{Buf: make([]byte, rsproto.DefaultReceiveBufferSize), Addr: 0x00007e1000000000, Key: 0x4ecf},
		GrantTable:       rsproto.Region{Buf: make([]byte, rsproto.SGLSize*rsproto.SGESize), Addr: 0x00007e2000000000, Key: 0x6a17},
		SendQueueSize:    rsproto.DefaultSendQueueSize,
		ReceiveQueueSize: rsproto.DefaultReceiveQueueSize,
	}
}

// testPeer is the connection data of a peer with the stock queues and buffers.
var testPeer = rsproto.ConnData{
	Credits:    rsproto.DefaultReceiveQueueSize,
	GrantTable: rsproto.SGE{Addr: 0x00007f2000000000, Key: 0x7a11, Length: rsproto.SGLSize},
	FirstGrant: rsproto.SGE{Addr: 0x00007f1000000000, Key: 0x7b22, Length: rsproto.DefaultReceiveBufferSize / 2},
}

// viewOffset returns where the view v starts in buf, failing t unless v lies
// inside buf and cannot be grown past its length.
func viewOffset(t *testing.T, buf, v []byte) int {
	t.Helper()
	if len(v) == 0 {
		t.Fatal("no view")
	}
	for off := range buf {
		if &buf[off] == &v[0] {
			if off+len(v) > len(buf) || cap(v) != len(v) {
				t.Fatalf("view [%d:%d] of capacity %d does not lie inside the send buffer of %d bytes", off, off+len(v), cap(v), len(buf))
			}
			return off
		}
	}
	t.Fatalf("view of %d bytes is not in the send buffer", len(v))
	return 0
}

// The send half of the borrow guarantee: where rsend copies into the send
// buffer at ssgl[0] for rs_write_data to post, Commit posts the view the
// caller filled, at its address and for the length committed.
func TestCommitPostsTheReservedView(t *testing.T) {
	c := testConfig()
	l := &recorder{}
	s := rsproto.NewStream(l, c)
	s.Connect(testPeer)

	for _, n := range []int{1000, 3000} {
		off := viewOffset(t, c.SendBuffer.Buf, s.Reserve())
		if err := s.Commit(n); err != nil {
			t.Fatalf("Commit(%d) = %v", n, err)
		}
		got := l.posts[len(l.posts)-1]
		sgl := []rsproto.Segment{{Addr: c.SendBuffer.Addr + uint64(off), Length: uint32(n), Key: c.SendBuffer.Key}}
		msg := rsproto.MakeMsg(rsproto.OpData, uint32(n))
		if got.op != "rs_post_write_msg" || !slices.Equal(got.sgl, sgl) || got.msg != msg {
			t.Errorf("Commit(%d) of the view at %d posted %+v, want rs_post_write_msg of %+v with message %#x", n, off, got, sgl, msg)
		}
	}
}

// rsend cuts each message at the remaining length of the current grant,
// target_sgl[target_sge], and rs_write_data writes it at the grant's address
// and moves the grant past it; rs_can_send waits while the grant is empty.
func TestMessageStaysWithinTheGrant(t *testing.T) {
	c := testConfig()
	peer := testPeer
	peer.FirstGrant.Length = 3000
	l := &recorder{}
	s := rsproto.NewStream(l, c)
	s.Connect(peer)

	var sent uint64
	for _, step := range []struct{ view, commit int }{{3000, 1000}, {2000, 2000}} {
		if v := s.Reserve(); len(v) != step.view {
			t.Fatalf("Reserve() after %d bytes into a grant of 3000 = %d bytes, want %d", sent, len(v), step.view)
		}
		if err := s.Commit(step.commit); err != nil {
			t.Fatalf("Commit(%d) = %v", step.commit, err)
		}
		if got := l.posts[len(l.posts)-1]; got.addr != peer.FirstGrant.Addr+sent || got.key != peer.FirstGrant.Key {
			t.Errorf("Commit(%d) wrote to %#x under %#x, want %#x under %#x", step.commit, got.addr, got.key, peer.FirstGrant.Addr+sent, peer.FirstGrant.Key)
		}
		sent += uint64(step.commit)
	}
	if s.CanSend() || s.Reserve() != nil {
		t.Errorf("CanSend() = %v, Reserve() = %d bytes with the grant used up; want false and none", s.CanSend(), len(s.Reserve()))
	}
}

// sendWhileCan commits one-byte messages while s can send, at most limit of
// them, and returns how many it sent.
func sendWhileCan(t *testing.T, s *rsproto.Stream, limit int) int {
	t.Helper()
	n := 0
	for ; n < limit && s.CanSend(); n++ {
		if v := s.Reserve(); len(v) == 0 {
			t.Fatal("CanSend() but Reserve() lends nothing")
		}
		if err := s.Commit(1); err != nil {
			t.Fatalf("Commit(1) = %v", err)
		}
	}
	return n
}

// rs_save_conn_data takes the peer's credits as sseq_comp, rs_can_send stops
// at sseq_no == sseq_comp, and rs_poll_cq takes the data of an RS_OP_SGL
// message, the peer's rseq_no + rq_size, as the new sseq_comp.
func TestCreditsRunOutAndReturn(t *testing.T) {
	peer := testPeer
	peer.Credits = 3
	s := rsproto.NewStream(&recorder{}, testConfig())
	s.Connect(peer)

	if n := sendWhileCan(t, s, 10); n != 3 {
		t.Fatalf("sent %d messages on 3 credits, want 3", n)
	}
	s.Received(rsproto.MakeMsg(rsproto.OpSGL, 5))
	if n := sendWhileCan(t, s, 10); n != 2 {
		t.Fatalf("sent %d more messages when the peer raised its credits to 5, want 2", n)
	}
}

// grant does what the peer's rs_send_credits does with a grant g: it writes g
// into entry i of the grant table of c, in the byte order of c's host, then
// sends the RS_OP_SGL message with the credit limit.
func grant(s *rsproto.Stream, c rsproto.Config, i int, g rsproto.SGE, credits uint32) {
	rsproto.PutSGE(c.GrantTable.Buf[i*rsproto.SGESize:], rsproto.ConnData{BigEndian: c.BigEndian}.ByteOrder(), g)
	s.Received(rsproto.MakeMsg(rsproto.OpSGL, credits))
}

// rs_write_data moves on to the next of the RS_SGL_SIZE entries of
// target_sgl when a grant is used up, and the peer's rs_send_credits writes
// its grants round-robin from entry 1 on, after the first one rs_save_conn_data
// puts in entry 0.
func TestGrantsRotateOverTheTable(t *testing.T) {
	c := testConfig()
	peer := testPeer
	peer.FirstGrant.Length = 100
	l := &recorder{}
	s := rsproto.NewStream(l, c)
	s.Connect(peer)

	grants := []struct {
		entry int
		g     rsproto.SGE
	}{
		{0, peer.FirstGrant},
		{1, rsproto.SGE{Addr: 0x00007f1000010000, Key: 0x7b23, Length: 200}},
		{0, rsproto.SGE{Addr: 0x00007f1000000000, Key: 0x7b24, Length: 300}},
	}
	for i, tt := range grants {
		if i > 0 {
			if s.CanSend() {
				t.Fatalf("CanSend() with grant %d used up and none in entry %d", i-1, tt.entry)
			}
			grant(s, c, tt.entry, tt.g, rsproto.DefaultReceiveQueueSize)
		}
		v := s.Reserve()
		if len(v) != int(tt.g.Length) {
			t.Fatalf("Reserve() with grant %d in entry %d = %d bytes, want %d", i, tt.entry, len(v), tt.g.Length)
		}
		if err := s.Commit(len(v)); err != nil {
			t.Fatalf("Commit(%d) = %v", len(v), err)
		}
		if got := l.posts[len(l.posts)-1]; got.addr != tt.g.Addr || got.key != tt.g.Key {
			t.Errorf("Commit into grant %d wrote to %#x under %#x, want %#x under %#x", i, got.addr, got.key, tt.g.Addr, tt.g.Key)
		}
	}
}

func TestStreamConnData(t *testing.T) {
	c := testConfig()
	little := rsproto.ConnData{
		Credits:    rsproto.DefaultReceiveQueueSize,
		GrantTable: rsproto.SGE{Addr: c.GrantTable.Addr, Key: c.GrantTable.Key, Length: rsproto.SGLSize},
		FirstGrant: rsproto.SGE{Addr: c.ReceiveBuffer.Addr, Key: c.ReceiveBuffer.Key, Length: rsproto.DefaultReceiveBufferSize / 2},
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
