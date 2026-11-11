package rsproto_test

import (
	"errors"
	"io"
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

// recorder is a link that records what a stream posts, the ids of receives
// apart from the sends, and answers each post with err.
type recorder struct {
	posts []post
	recvs []uint64
	err   error
}

func (r *recorder) PostWriteMsg(id uint64, sgl []rsproto.Segment, msg rsproto.Msg, addr uint64, key uint32) error {
	r.posts = append(r.posts, post{"rs_post_write_msg", id, slices.Clone(sgl), msg, addr, key})
	return r.err
}

func (r *recorder) PostMsg(id uint64, msg rsproto.Msg) error {
	r.posts = append(r.posts, post{op: "rs_post_msg", id: id, msg: msg})
	return r.err
}

func (r *recorder) PostRecv(id uint64) error {
	r.recvs = append(r.recvs, id)
	return r.err
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

// The peer's rs_send_credits writes a grant in the byte order of this host,
// RS_CONN_FLAG_NET telling it, and rs_write_data reads target_sgl natively.
func TestGrantTableInHostByteOrder(t *testing.T) {
	c := testConfig()
	c.BigEndian = true
	peer := testPeer
	peer.FirstGrant.Length = 100
	l := &recorder{}
	s := rsproto.NewStream(l, c)
	s.Connect(peer)
	if err := s.Commit(len(s.Reserve())); err != nil {
		t.Fatalf("Commit of the first grant = %v", err)
	}

	second := rsproto.SGE{Addr: 0x00007f1000010000, Key: 0x7b23, Length: 200}
	grant(s, c, 1, second, rsproto.DefaultReceiveQueueSize)
	v := s.Reserve()
	if len(v) != int(second.Length) {
		t.Fatalf("Reserve() on a big-endian host with %+v in entry 1 = %d bytes, want %d", second, len(v), second.Length)
	}
	if err := s.Commit(len(v)); err != nil {
		t.Fatalf("Commit(%d) = %v", len(v), err)
	}
	if got := l.posts[len(l.posts)-1]; got.addr != second.Addr || got.key != second.Key {
		t.Errorf("Commit on a big-endian host wrote to %#x under %#x, want %#x under %#x", got.addr, got.key, second.Addr, second.Key)
	}
}

// rs_init_bufs leaves sq_size - RS_QP_CTRL_SIZE send queue entries for data,
// rs_write_data takes one per message, and rs_poll_cq gives one back for each
// completed data send.
func TestCompletionGivesBackQueueEntry(t *testing.T) {
	c := testConfig()
	c.SendQueueSize = rsproto.QPMinSize
	l := &recorder{}
	s := rsproto.NewStream(l, c)
	s.Connect(testPeer)

	if n, want := sendWhileCan(t, s, 100), rsproto.QPMinSize-rsproto.QPCtrlSize; n != want {
		t.Fatalf("sent %d messages on a send queue of %d entries, want %d", n, rsproto.QPMinSize, want)
	}
	s.Sent(l.posts[0].id, nil)
	if n := sendWhileCan(t, s, 100); n != 1 {
		t.Fatalf("sent %d messages after one completion, want 1", n)
	}
}

// rs_write_data takes the bytes of a message from sbuf_bytes_avail and
// rs_poll_cq gives them back when the send completes, rs_can_send waiting for
// RS_SNDLOWAT of them; the free bytes run on from ssgl[0], which rsend moves
// back to the start of the send buffer at its end.
func TestCompletionGivesBackBufferBytes(t *testing.T) {
	c := testConfig()
	buf := make([]byte, 2*rsproto.SndLowat)
	c.SendBuffer.Buf = buf
	l := &recorder{}
	s := rsproto.NewStream(l, c)
	s.Connect(testPeer)

	first := rsproto.SndLowat + 1
	s.Reserve()
	if err := s.Commit(first); err != nil {
		t.Fatalf("Commit(%d) = %v", first, err)
	}
	if s.CanSend() {
		t.Fatalf("CanSend() with %d of %d bytes free", len(buf)-first, len(buf))
	}
	s.Sent(l.posts[0].id, nil)
	v := s.Reserve()
	if off := viewOffset(t, buf, v); off != first || len(v) != len(buf)-first {
		t.Fatalf("Reserve() after the first message completed = [%d:%d], want the rest of the buffer [%d:%d]", off, off+len(v), first, len(buf))
	}
	if err := s.Commit(len(v)); err != nil {
		t.Fatalf("Commit(%d) = %v", len(v), err)
	}
	v = s.Reserve()
	if off := viewOffset(t, buf, v); off != 0 || len(v) != first {
		t.Fatalf("Reserve() with [%d:%d] in flight = [%d:%d], want [0:%d]", first, len(buf), off, off+len(v), first)
	}
}

// sseq_no and sseq_comp are 16 bits: rs_poll_cq cuts to them the peer's
// credit limit, which rs_send_credits sums as rseq_no + rq_size past 16 bits,
// and rs_can_send compares them as they wrap.
func TestSequenceWrapsPast65536Messages(t *testing.T) {
	c := testConfig()
	c.SendBuffer.Buf = make([]byte, 2*rsproto.SndLowat)
	peer := testPeer
	peer.Credits = rsproto.QPMinSize
	peer.FirstGrant.Length = 1 << 20
	l := &recorder{}
	s := rsproto.NewStream(l, c)
	s.Connect(peer)

	for sent := 0; sent < 1<<16+4*rsproto.QPMinSize; {
		n := sendWhileCan(t, s, 2*rsproto.QPMinSize)
		if n != rsproto.QPMinSize {
			t.Fatalf("sent %d messages after %d on credits for %d more, want %d", n, sent, rsproto.QPMinSize, rsproto.QPMinSize)
		}
		sent += n
		for _, p := range l.posts {
			s.Sent(p.id, nil)
		}
		l.posts = l.posts[:0]
		s.Received(rsproto.MakeMsg(rsproto.OpSGL, uint32(uint16(sent))+rsproto.QPMinSize))
	}
}

// rs_poll_cq turns a failed send of a connected rsocket into rs_error with
// err EIO, where an orderly end of the peer leaves err 0.
func TestFailedSendIsAnError(t *testing.T) {
	l := &recorder{}
	s := rsproto.NewStream(l, testConfig())
	s.Connect(testPeer)
	sendWhileCan(t, s, 2)

	s.Sent(l.posts[0].id, nil)
	if err := s.Err(); err != nil {
		t.Fatalf("Err() after a completed send = %v, want nil", err)
	}
	failed := errors.New("IBV_WC_RETRY_EXC_ERR")
	s.Sent(l.posts[1].id, failed)
	if err := s.Err(); !errors.Is(err, failed) || errors.Is(err, io.EOF) {
		t.Errorf("Err() after a failed send = %v, want an error wrapping %q, not the io.EOF of an orderly end", err, failed)
	}
}

// rs_poll_cq leaves rs_connected at the first failed send, so the sends
// flushed after it change neither the state nor err.
func TestFirstFailedSendStaysTheError(t *testing.T) {
	l := &recorder{}
	s := rsproto.NewStream(l, testConfig())
	s.Connect(testPeer)
	sendWhileCan(t, s, 2)

	failed, flushed := errors.New("IBV_WC_RETRY_EXC_ERR"), errors.New("IBV_WC_WR_FLUSH_ERR")
	s.Sent(l.posts[0].id, failed)
	s.Sent(l.posts[1].id, flushed)
	if err := s.Err(); !errors.Is(err, failed) || errors.Is(err, flushed) {
		t.Errorf("Err() after a failed send and a flushed one = %v, want the failure %q", err, failed)
	}
}

// rsend sends no message of more than RS_MAX_TRANSFER bytes, however much
// room the grant and the send buffer leave.
func TestMessageStaysWithinMaxTransfer(t *testing.T) {
	c := testConfig()
	c.SendBuffer.Buf = make([]byte, 4*rsproto.MaxTransfer)
	peer := testPeer
	peer.FirstGrant.Length = 4 * rsproto.MaxTransfer
	s := rsproto.NewStream(&recorder{}, c)
	s.Connect(peer)

	if v := s.Reserve(); len(v) != rsproto.MaxTransfer {
		t.Errorf("Reserve() with room for %d bytes = %d bytes, want RS_MAX_TRANSFER, %d", 4*rsproto.MaxTransfer, len(v), rsproto.MaxTransfer)
	}
}

// rsend of no bytes sends no message, so a commit of none takes no credit and
// posts nothing.
func TestCommitOfNothingPostsNothing(t *testing.T) {
	peer := testPeer
	peer.Credits = 1
	l := &recorder{}
	s := rsproto.NewStream(l, testConfig())
	s.Connect(peer)

	s.Reserve()
	if err := s.Commit(0); err != nil {
		t.Fatalf("Commit(0) = %v", err)
	}
	if len(l.posts) != 0 || !s.CanSend() {
		t.Errorf("Commit(0) on 1 credit posted %+v and left CanSend() = %v, want nothing posted and true", l.posts, s.CanSend())
	}
}

// rs_init_bufs gets the grant table zeroed from forksafe_alloc, so an entry
// no grant has reached is empty, whatever the memory held before.
func TestNewStreamClearsTheGrantTable(t *testing.T) {
	c := testConfig()
	for i := range c.GrantTable.Buf {
		c.GrantTable.Buf[i] = 0xff
	}
	peer := testPeer
	peer.FirstGrant.Length = 100
	s := rsproto.NewStream(&recorder{}, c)
	s.Connect(peer)

	if err := s.Commit(len(s.Reserve())); err != nil {
		t.Fatalf("Commit of the first grant = %v", err)
	}
	if s.CanSend() {
		t.Errorf("CanSend() with the first grant used up and no grant written into a table that held 0xff bytes")
	}
}

// rsend returns the error of the post rs_write_data could not make.
func TestCommitReturnsThePostError(t *testing.T) {
	refused := errors.New("ibv_post_send: ENOMEM")
	s := rsproto.NewStream(&recorder{err: refused}, testConfig())
	s.Connect(testPeer)

	s.Reserve()
	if err := s.Commit(1); !errors.Is(err, refused) {
		t.Errorf("Commit(1) with the post refused = %v, want %v", err, refused)
	}
}

// rs_create_ep posts rq_size receives before the connection is made, each
// with the id rs_recv_wr_id(0) by which rs_poll_cq tells them from sends.
func TestNewStreamPostsTheReceiveQueue(t *testing.T) {
	l := &recorder{}
	rsproto.NewStream(l, testConfig())

	if want := slices.Repeat([]uint64{1 << 63}, rsproto.DefaultReceiveQueueSize); !slices.Equal(l.recvs, want) {
		t.Errorf("NewStream posted %d receives %#x, want %d with id %#x", len(l.recvs), l.recvs, len(want), uint64(1<<63))
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
