package rsproto_test

import (
	"bytes"
	"encoding/binary"
	"errors"
	"io"
	"math/rand/v2"
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

// data returns the posts of data messages.
func (r *recorder) data() []post {
	var d []post
	for _, p := range r.posts {
		if p.msg.Op() == rsproto.OpData {
			d = append(d, p)
		}
	}
	return d
}

// testConfig describes local memory of the stock sizes as an engine registers
// it, at addresses and under keys that tell the blocks apart.
func testConfig() rsproto.Config {
	return rsproto.Config{
		SendBuffer:       rsproto.Region{Buf: make([]byte, rsproto.DefaultSendBufferSize), Addr: 0x00007e0000000000, Key: 0x5e4d},
		ReceiveBuffer:    rsproto.Region{Buf: make([]byte, rsproto.DefaultReceiveBufferSize), Addr: 0x00007e1000000000, Key: 0x4ecf},
		GrantTable:       rsproto.Region{Buf: make([]byte, rsproto.SGLSize*rsproto.SGESize), Addr: 0x00007e2000000000, Key: 0x6a17},
		ControlBuffer:    rsproto.Region{Buf: make([]byte, rsproto.QPCtrlSize*rsproto.SGESize), Addr: 0x00007e3000000000, Key: 0x5e4e},
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
				t.Fatalf("view [%d:%d] of capacity %d does not lie inside the buffer of %d bytes", off, off+len(v), cap(v), len(buf))
			}
			return off
		}
	}
	t.Fatalf("view of %d bytes is not in the buffer", len(v))
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
	s.Sent(l.data()[0].id, nil)
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
	s.Sent(l.data()[0].id, nil)
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

	s.Sent(l.data()[0].id, nil)
	if err := s.Err(); err != nil {
		t.Fatalf("Err() after a completed send = %v, want nil", err)
	}
	failed := errors.New("IBV_WC_RETRY_EXC_ERR")
	s.Sent(l.data()[1].id, failed)
	if err := s.Err(); s.State() != rsproto.StateError || !errors.Is(err, failed) || errors.Is(err, io.EOF) {
		t.Errorf("a failed send left the state %#x and Err() = %v, want %#x and an error wrapping %q, not the io.EOF of an orderly end", s.State(), err, rsproto.StateError, failed)
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
	s.Sent(l.data()[0].id, failed)
	s.Sent(l.data()[1].id, flushed)
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
	if len(l.data()) != 0 || !s.CanSend() {
		t.Errorf("Commit(0) on 1 credit posted %+v and left CanSend() = %v, want nothing posted and true", l.data(), s.CanSend())
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

// rs_poll_cq posts a receive for every message that arrived, whatever it
// carries, in place of the one the message used up.
func TestEveryArrivalPostsAReceive(t *testing.T) {
	l := &recorder{}
	s := rsproto.NewStream(l, testConfig())
	s.Connect(testPeer)
	l.recvs = nil

	s.Received(rsproto.MakeMsg(rsproto.OpData, 100))
	s.Received(rsproto.MakeMsg(rsproto.OpSGL, rsproto.DefaultReceiveQueueSize))
	if want := []uint64{1 << 63, 1 << 63}; !slices.Equal(l.recvs, want) {
		t.Errorf("a data and a credit message arriving posted receives %#x, want %#x", l.recvs, want)
	}
}

// peerWrites does what the peer's rs_write_data does with b: it writes b at
// off into the receive buffer of c, and the message for it arrives at s.
func peerWrites(s *rsproto.Stream, c rsproto.Config, off int, b []byte) {
	copy(c.ReceiveBuffer.Buf[off:], b)
	s.Received(rsproto.MakeMsg(rsproto.OpData, uint32(len(b))))
}

// readAll reads what has arrived at s through the views Peek lends, at most
// chunk bytes of each, failing t unless every view lies inside rbuf.
func readAll(t *testing.T, s *rsproto.Stream, rbuf []byte, chunk int) []byte {
	t.Helper()
	var got []byte
	for v := s.Peek(); len(v) > 0; v = s.Peek() {
		viewOffset(t, rbuf, v)
		n := min(len(v), chunk)
		got = append(got, v[:n]...)
		s.Consume(n)
	}
	return got
}

// The receive half of the borrow guarantee: rrecv takes the bytes of the
// messages rs_poll_cq queued in their order from rbuf_offset on, going on at
// the start of the receive buffer past its end, where the peer's grants, the
// halves of the buffer in turn, have put them.
func TestViewsComeInArrivalOrderAcrossTheWrap(t *testing.T) {
	c := testConfig()
	s := rsproto.NewStream(&recorder{}, c)
	s.Connect(testPeer)
	rbuf := c.ReceiveBuffer.Buf
	half := len(rbuf) / 2
	rng := rand.NewChaCha8([32]byte{})

	var sent, got []byte
	for _, round := range [][]struct{ off, n int }{
		{{0, 20000}, {20000, half - 20000}},
		{{half, 30000}, {half + 30000, half - 30000}, {0, 1000}},
	} {
		for _, m := range round {
			b := make([]byte, m.n)
			rng.Read(b)
			peerWrites(s, c, m.off, b)
			sent = append(sent, b...)
		}
		got = append(got, readAll(t, s, rbuf, 25000)...)
	}
	if !bytes.Equal(got, sent) {
		t.Errorf("the views lent %d bytes, not the %d bytes the peer wrote in order", len(got), len(sent))
	}
}

// rs_poll_cq queues as data the message of every op but RS_OP_SGL,
// RS_OP_IOMAP_SGL, RS_OP_CTRL and RS_OP_WRITE: the reserved ops 1, 3 and 5
// too.
func TestWhichOpsArriveAsData(t *testing.T) {
	for op := range rsproto.Op(8) {
		s := rsproto.NewStream(&recorder{}, testConfig())
		s.Connect(testPeer)
		s.Received(rsproto.MakeMsg(op, 100))

		want := 0
		if op == rsproto.OpData || op == 1 || op == 3 || op == 5 {
			want = 100
		}
		if got := len(s.Peek()); got != want {
			t.Errorf("a message of op %d and data 100 lent a view of %d bytes, want %d", op, got, want)
		}
	}
}

// grantPosted returns the grant entry p writes into the peer's grant table,
// failing t unless p is an rs_post_write_msg of one entry from the control
// buffer of c.
func grantPosted(t *testing.T, c rsproto.Config, p post) [rsproto.SGESize]byte {
	t.Helper()
	ctrl := c.ControlBuffer
	if p.op != "rs_post_write_msg" || len(p.sgl) != 1 || p.sgl[0].Length != rsproto.SGESize || p.sgl[0].Key != ctrl.Key ||
		p.sgl[0].Addr < ctrl.Addr || p.sgl[0].Addr+rsproto.SGESize > ctrl.Addr+uint64(len(ctrl.Buf)) {
		t.Fatalf("%+v is no write of a grant entry from the control buffer at %#x under %#x", p, ctrl.Addr, ctrl.Key)
	}
	return [rsproto.SGESize]byte(ctrl.Buf[p.sgl[0].Addr-ctrl.Addr:])
}

// rs_init_bufs leaves the second half of the receive buffer for the first
// rs_update_credits of the connection to grant: rs_send_credits writes it
// into entry 1 of the peer's grant table, remote_sge, with RS_OP_SGL
// carrying the credit limit rseq_no + rq_size.
func TestConnectGrantsTheSecondHalf(t *testing.T) {
	c := testConfig()
	l := &recorder{}
	s := rsproto.NewStream(l, c)
	s.Connect(testPeer)

	if len(l.posts) != 1 {
		t.Fatalf("Connect posted %+v, want one grant", l.posts)
	}
	p := l.posts[0]
	msg, entry1 := rsproto.MakeMsg(rsproto.OpSGL, rsproto.DefaultReceiveQueueSize), testPeer.GrantTable.Addr+rsproto.SGESize
	if p.msg != msg || p.addr != entry1 || p.key != testPeer.GrantTable.Key {
		t.Errorf("Connect wrote message %#x to %#x under %#x, want %#x to entry 1 of the peer's grant table, %#x under %#x", p.msg, p.addr, p.key, msg, entry1, testPeer.GrantTable.Key)
	}
	half := uint32(len(c.ReceiveBuffer.Buf) / 2)
	b := grantPosted(t, c, p)
	if got, want := rsproto.GetSGE(b[:], binary.LittleEndian), (rsproto.SGE{Addr: c.ReceiveBuffer.Addr + uint64(half), Key: c.ReceiveBuffer.Key, Length: half}); got != want {
		t.Errorf("Connect granted %+v, want the second half of the receive buffer, %+v", got, want)
	}
}

// rs_send_credits writes the grant entry in host order, swapped when
// rs_save_conn_data found the peer's RS_CONN_FLAG_NET at odds with the host
// (RS_OPT_SWAP_SGL): in the peer's order either way.
func TestGrantInThePeerByteOrder(t *testing.T) {
	// The second half of testConfig's receive buffer, {0x00007e1000010000, 0x4ecf, 0x10000}.
	big := [rsproto.SGESize]byte{0x00, 0x00, 0x7e, 0x10, 0x00, 0x01, 0x00, 0x00, 0x00, 0x00, 0x4e, 0xcf, 0x00, 0x01, 0x00, 0x00}
	little := [rsproto.SGESize]byte{0x00, 0x00, 0x01, 0x00, 0x10, 0x7e, 0x00, 0x00, 0xcf, 0x4e, 0x00, 0x00, 0x00, 0x00, 0x01, 0x00}
	tests := []struct {
		c               string // the C function and case that writes the entry
		host, peerIsBig bool
		wire            [rsproto.SGESize]byte
	}{
		{"rs_send_credits on a little-endian host for a big-endian peer, swapped", false, true, big},
		{"rs_send_credits on a big-endian host for a little-endian peer, swapped", true, false, little},
		{"rs_send_credits on a big-endian host for a big-endian peer", true, true, big},
	}
	for _, tt := range tests {
		t.Run(tt.c, func(t *testing.T) {
			c := testConfig()
			c.BigEndian = tt.host
			peer := testPeer
			peer.BigEndian = tt.peerIsBig
			l := &recorder{}
			s := rsproto.NewStream(l, c)
			s.Connect(peer)

			if got := grantPosted(t, c, l.posts[0]); got != tt.wire {
				t.Errorf("Connect granted % x, want % x", got, tt.wire)
			}
		})
	}
}

// rrecv adds the bytes it takes to rbuf_bytes_avail, and rs_give_credits
// grants room again once half the receive buffer is free.
func TestReadingHalfTheBufferEarnsAGrant(t *testing.T) {
	c := testConfig()
	l := &recorder{}
	s := rsproto.NewStream(l, c)
	s.Connect(testPeer)
	half := len(c.ReceiveBuffer.Buf) / 2
	peerWrites(s, c, 0, make([]byte, half))

	s.Peek()
	s.Consume(half - 1)
	if len(l.posts) != 1 {
		t.Fatalf("reading %d bytes posted %+v after the grant of Connect, want nothing", half-1, l.posts[1:])
	}
	s.Peek()
	s.Consume(1)
	if len(l.posts) != 2 {
		t.Fatalf("reading half the receive buffer, %d bytes, posted %+v after the grant of Connect, want a grant", half, l.posts[1:])
	}
	grantPosted(t, c, l.posts[1])
}

// rs_send_credits grants the halves of the receive buffer in turn from
// rbuf_free_offset on, into the entries of the peer's grant table in turn
// from remote_sge 1 on, wrapping at the count of entries the peer sent.
func TestGrantsRotateOverHalvesAndEntries(t *testing.T) {
	tests := []struct {
		c       string // the C function and case that rotates the grants
		entries uint32
		want    []int // the entry each grant goes to
	}{
		{"rs_send_credits for a peer of RS_SGL_SIZE entries", rsproto.SGLSize, []int{1, 0, 1}},
		{"rs_send_credits for a peer of 3 entries", 3, []int{1, 2, 0}},
	}
	for _, tt := range tests {
		t.Run(tt.c, func(t *testing.T) {
			c := testConfig()
			peer := testPeer
			peer.GrantTable.Length = tt.entries
			l := &recorder{}
			s := rsproto.NewStream(l, c)
			s.Connect(peer)
			half := len(c.ReceiveBuffer.Buf) / 2
			granted := func(p post) rsproto.SGE {
				b := grantPosted(t, c, p)
				return rsproto.GetSGE(b[:], binary.LittleEndian)
			}
			grants := []rsproto.SGE{granted(l.posts[0])}
			for _, off := range []int{0, half} {
				peerWrites(s, c, off, make([]byte, half))
				s.Peek()
				s.Consume(half)
				grants = append(grants, granted(l.posts[len(l.posts)-1]))
			}

			if len(l.posts) != len(tt.want) {
				t.Fatalf("Connect and reading the whole receive buffer posted %d messages, want %d grants", len(l.posts), len(tt.want))
			}
			for i, entry := range tt.want {
				wantAddr, wantGrant := peer.GrantTable.Addr+uint64(entry)*rsproto.SGESize, c.ReceiveBuffer.Addr+uint64(half*(1-i%2))
				if l.posts[i].addr != wantAddr || grants[i].Addr != wantGrant {
					t.Errorf("grant %d gave %#x in %#x, want %#x in entry %d, %#x", i, grants[i].Addr, l.posts[i].addr, wantGrant, entry, wantAddr)
				}
			}
		})
	}
}

// rs_send_credits copies each grant entry into its own slot of the control
// buffer, rs_get_ctrl_buf of ctrl_seqno, so that a grant in flight keeps its
// bytes while the next ones are posted.
func TestGrantsInFlightKeepTheirBytes(t *testing.T) {
	c := testConfig()
	l := &recorder{}
	s := rsproto.NewStream(l, c)
	s.Connect(testPeer)
	first := grantPosted(t, c, l.posts[0])
	half := len(c.ReceiveBuffer.Buf) / 2
	peerWrites(s, c, 0, make([]byte, half))
	s.Peek()
	s.Consume(half)

	if got := grantPosted(t, c, l.posts[0]); got != first {
		t.Errorf("the grant of Connect, in flight, changed from % x to % x when the next was posted", first, got)
	}
}

// readMessages reads n messages of one byte each that arrive at s one after
// the other, the first at off of the receive buffer of c.
func readMessages(s *rsproto.Stream, c rsproto.Config, off, n int) {
	for i := range n {
		peerWrites(s, c, off+i, []byte{byte(i)})
		s.Peek()
		s.Consume(1)
	}
}

// isCredit reports whether p is the bare credit message rs_send_credits posts
// with rs_post_msg, RS_OP_SGL carrying the credit limit credits.
func isCredit(p post, credits uint32) bool {
	return p.op == "rs_post_msg" && p.msg == rsproto.MakeMsg(rsproto.OpSGL, credits)
}

// rs_give_credits gives credits too once rseq_no, the messages rrecv has
// read, reaches rseq_comp, rq_size/2 past rseq_no at the last credits; with
// less than half the receive buffer free, rs_send_credits sends them bare.
func TestCreditsBelowHalfABufferGoBare(t *testing.T) {
	c := testConfig()
	c.ReceiveQueueSize = rsproto.QPMinSize
	l := &recorder{}
	s := rsproto.NewStream(l, c)
	s.Connect(testPeer)

	readMessages(s, c, 0, rsproto.QPMinSize/2-1)
	if len(l.posts) != 1 {
		t.Fatalf("reading %d messages posted %+v after the grant of Connect, want nothing", rsproto.QPMinSize/2-1, l.posts[1:])
	}
	readMessages(s, c, rsproto.QPMinSize/2-1, 1)
	if want := uint32(rsproto.QPMinSize/2 + rsproto.QPMinSize); len(l.posts) != 2 || !isCredit(l.posts[1], want) {
		t.Errorf("reading rq_size/2, %d, messages posted %+v after the grant of Connect, want credits for %d", rsproto.QPMinSize/2, l.posts[1:], want)
	}
}

// rrecv counts a message into rseq_no only once it has taken all of it,
// rmsg[rmsg_head].data shrinking by what it takes before.
func TestPartlyReadMessageEarnsNoCredit(t *testing.T) {
	c := testConfig()
	c.ReceiveQueueSize = rsproto.QPMinSize
	l := &recorder{}
	s := rsproto.NewStream(l, c)
	s.Connect(testPeer)
	n := rsproto.QPMinSize / 2
	for i := range n {
		peerWrites(s, c, 2*i, []byte{byte(i), byte(i)})
	}

	s.Peek()
	s.Consume(2*n - 1)
	if len(l.posts) != 1 {
		t.Fatalf("reading %d messages and a byte of the next posted %+v after the grant of Connect, want nothing", n-1, l.posts[1:])
	}
	s.Peek()
	s.Consume(1)
	if want := uint32(n + rsproto.QPMinSize); len(l.posts) != 2 || !isCredit(l.posts[1], want) {
		t.Errorf("reading the last byte of %d messages posted %+v after the grant of Connect, want credits for %d", n, l.posts[1:], want)
	}
}

// readCompleting reads n messages of one byte as readMessages does, each
// post the stream makes meanwhile completing at once, and returns the last.
func readCompleting(s *rsproto.Stream, c rsproto.Config, l *recorder, n int) post {
	var last post
	for i := range n {
		readMessages(s, c, i%len(c.ReceiveBuffer.Buf), 1)
		for _, p := range l.posts {
			s.Sent(p.id, nil)
			last = p
		}
		l.posts = l.posts[:0]
	}
	return last
}

// rs_send_credits sends rseq_no + rq_size as the int it sums to, past 16
// bits, and the peer's rs_poll_cq cuts it to its uint16_t sseq_comp.
func TestCreditLimitIsNotCutTo16Bits(t *testing.T) {
	c := testConfig()
	c.ReceiveQueueSize = rsproto.QPMinSize
	l := &recorder{}
	s := rsproto.NewStream(l, c)
	s.Connect(testPeer)

	read := 1<<16 - rsproto.QPMinSize
	if last, want := readCompleting(s, c, l, read), uint32(1<<16); !isCredit(last, want) {
		t.Errorf("the credits given after reading %d messages are %+v, want %#x", read, last, want)
	}
}

// rs_give_credits compares rseq_no with rseq_comp as signed 16-bit numbers,
// so credits keep coming every rq_size/2 messages as both wrap.
func TestCreditsKeepComingAsTheSequenceWraps(t *testing.T) {
	c := testConfig()
	c.ReceiveQueueSize = rsproto.QPMinSize
	l := &recorder{}
	s := rsproto.NewStream(l, c)
	s.Connect(testPeer)
	s.Sent(l.posts[0].id, nil)
	l.posts = nil

	for read := 1; read <= 1<<16+4*rsproto.QPMinSize; read++ {
		readMessages(s, c, read%len(c.ReceiveBuffer.Buf), 1)
		due := read%(rsproto.QPMinSize/2) == 0
		msg := rsproto.MakeMsg(rsproto.OpSGL, uint32(uint16(read))+rsproto.QPMinSize)
		if due && (len(l.posts) != 1 || l.posts[0].msg != msg) || !due && len(l.posts) != 0 {
			t.Fatalf("reading message %d posted %+v, want credits %v (%#x)", read, l.posts, due, msg)
		}
		for _, p := range l.posts {
			s.Sent(p.id, nil)
		}
		l.posts = l.posts[:0]
	}
}

// fourCreditsDue returns a stream that has posted RS_QP_CTRL_SIZE grants and
// credits, none of them completed, and is due a fifth.
func fourCreditsDue(t *testing.T) (*rsproto.Stream, *recorder) {
	t.Helper()
	c := testConfig()
	c.ReceiveQueueSize = rsproto.QPMinSize
	l := &recorder{}
	s := rsproto.NewStream(l, c)
	s.Connect(testPeer)
	readMessages(s, c, 0, rsproto.QPCtrlSize*rsproto.QPMinSize/2)
	return s, l
}

// rs_give_credits waits for rs_ctrl_avail, ctrl_seqno short of
// ctrl_max_seqno, which rs_alloc starts at RS_QP_CTRL_SIZE.
func TestAtMostFourCreditsInFlight(t *testing.T) {
	_, l := fourCreditsDue(t)
	if len(l.posts) != rsproto.QPCtrlSize {
		t.Errorf("Connect and reading %d messages posted %d grants and credits with none completed, want %d", rsproto.QPCtrlSize*rsproto.QPMinSize/2, len(l.posts), rsproto.QPCtrlSize)
	}
}

// rs_poll_cq gives a control slot back, ctrl_max_seqno++, when an RS_OP_SGL
// send completes, and rs_process_cq then sends the credits held for it.
func TestCreditCompletionGivesBackItsSlot(t *testing.T) {
	s, l := fourCreditsDue(t)
	s.Sent(l.posts[0].id, nil)
	if want := uint32(rsproto.QPCtrlSize*rsproto.QPMinSize/2 + rsproto.QPMinSize); len(l.posts) != rsproto.QPCtrlSize+1 || !isCredit(l.posts[rsproto.QPCtrlSize], want) {
		t.Errorf("the completion of the grant of Connect posted %+v, want the credits for %d held for it", l.posts[rsproto.QPCtrlSize:], want)
	}
}

// rs_poll_cq gives a send queue entry and send buffer bytes back for a data
// send that completes, not for an RS_OP_SGL one, which took neither.
func TestCreditCompletionGivesBackNoQueueEntry(t *testing.T) {
	c := testConfig()
	c.SendQueueSize = rsproto.QPMinSize
	l := &recorder{}
	s := rsproto.NewStream(l, c)
	s.Connect(testPeer)
	sendWhileCan(t, s, 100)

	s.Sent(l.posts[0].id, nil)
	if s.CanSend() {
		t.Errorf("CanSend() after the grant of Connect completed with the send queue full of data, want false")
	}
}

// rs_give_credits gives none before the rsocket is rs_connected, when there
// is no grant table of the peer to write to yet.
func TestNoCreditsBeforeConnect(t *testing.T) {
	c := testConfig()
	l := &recorder{}
	s := rsproto.NewStream(l, c)
	half := len(c.ReceiveBuffer.Buf) / 2
	peerWrites(s, c, 0, make([]byte, half))
	s.Peek()
	s.Consume(half)

	if len(l.posts) != 0 {
		t.Errorf("reading half the receive buffer before Connect posted %+v, want nothing", l.posts)
	}
}

// rs_create_ep fails at the first receive rs_post_recv cannot post.
func TestRefusedReceiveFailsNewStream(t *testing.T) {
	refused := errors.New("ibv_post_recv: ENOMEM")
	l := &recorder{err: refused}
	s := rsproto.NewStream(l, testConfig())

	if err := s.Err(); !errors.Is(err, refused) || len(l.recvs) != 1 {
		t.Errorf("NewStream on a link refusing receives posted %d and left Err() = %v, want 1 and an error wrapping %q", len(l.recvs), err, refused)
	}
}

// rs_poll_cq turns a receive it cannot post in place of an arrival into
// rs_error with errno.
func TestRefusedReceiveBreaksTheStream(t *testing.T) {
	refused := errors.New("ibv_post_recv: EINVAL")
	l := &recorder{}
	s := rsproto.NewStream(l, testConfig())
	s.Connect(testPeer)
	l.err = refused
	s.Received(rsproto.MakeMsg(rsproto.OpData, 100))

	if err := s.Err(); !errors.Is(err, refused) {
		t.Errorf("Err() after the receive in place of an arrival was refused = %v, want an error wrapping %q", err, refused)
	}
}

// rs_send_credits leaves a grant or credits the link refuses to show at a
// later post; here the stream breaks at once.
func TestRefusedCreditBreaksTheStream(t *testing.T) {
	tests := []struct {
		c              string // the post of rs_send_credits refused
		rqSize         int
		messages, size int // what is read
	}{
		{"rs_post_write_msg of the grant for half the receive buffer", rsproto.DefaultReceiveQueueSize, 1, rsproto.DefaultReceiveBufferSize / 2},
		{"rs_post_msg of the credits for rq_size/2 messages", rsproto.QPMinSize, rsproto.QPMinSize / 2, 1},
	}
	for _, tt := range tests {
		t.Run(tt.c, func(t *testing.T) {
			refused := errors.New("ibv_post_send: EINVAL")
			c := testConfig()
			c.ReceiveQueueSize = tt.rqSize
			l := &recorder{}
			s := rsproto.NewStream(l, c)
			s.Connect(testPeer)
			for i := range tt.messages {
				peerWrites(s, c, i*tt.size, make([]byte, tt.size))
			}
			s.Peek()
			l.err = refused
			s.Consume(tt.messages * tt.size)

			if err := s.Err(); len(l.posts) != 2 || !errors.Is(err, refused) {
				t.Errorf("Err() after %d posts were refused = %v, want 1 and an error wrapping %q", len(l.posts)-1, err, refused)
			}
		})
	}
}

// rs_do_connect and rs_accept leave a connection rs_connect_rdwr, and
// rs_poll_cq ignores the control data it does not know, RS_CTRL_KEEPALIVE
// among them, but for the receive it posts in place of the one used up.
func TestKeepaliveAndUnknownControlDataAreIgnored(t *testing.T) {
	for _, ctrl := range []uint32{rsproto.CtrlKeepalive, 3, 0x1FFFFFFF} {
		l := &recorder{}
		s := rsproto.NewStream(l, testConfig())
		s.Connect(testPeer)
		l.posts, l.recvs = nil, nil
		s.Received(rsproto.MakeMsg(rsproto.OpCtrl, ctrl))

		if want := rsproto.StateConnected | rsproto.StateReadable | rsproto.StateWritable; s.State() != want || len(l.posts) != 0 || len(l.recvs) != 1 {
			t.Errorf("control data %d left the state %#x, posted %+v and %d receives; want %#x, nothing and 1 receive", ctrl, s.State(), l.posts, len(l.recvs), want)
		}
	}
}

// isCtrl reports whether p is the control message rshutdown posts with
// rs_post_msg, RS_OP_CTRL carrying ctrl.
func isCtrl(p post, ctrl uint32) bool {
	return p.op == "rs_post_msg" && p.msg == rsproto.MakeMsg(rsproto.OpCtrl, ctrl)
}

// rshutdown(SHUT_WR) of a connected rsocket that is still readable clears
// rs_writable only and posts RS_CTRL_SHUTDOWN; rs_poll_cq goes on taking the
// data that arrives and posting receives for it.
func TestCloseWriteSendsShutdownAndReadsGoOn(t *testing.T) {
	c := testConfig()
	l := &recorder{}
	s := rsproto.NewStream(l, c)
	s.Connect(testPeer)
	l.posts, l.recvs = nil, nil

	s.CloseWrite()
	if len(l.posts) != 1 || !isCtrl(l.posts[0], rsproto.CtrlShutdown) {
		t.Fatalf("CloseWrite posted %+v, want RS_CTRL_SHUTDOWN", l.posts)
	}
	peerWrites(s, c, 0, []byte("after"))
	if want := rsproto.StateConnected | rsproto.StateReadable; s.State() != want || string(s.Peek()) != "after" || len(l.recvs) != 1 {
		t.Errorf("after CloseWrite the state is %#x, and data arriving was lent as %q with %d receives posted; want %#x, %q and 1", s.State(), s.Peek(), len(l.recvs), want, "after")
	}
}

// rsend fails with ECONNRESET once rs_writable is clear, though only when
// rs_can_send fails first; the stream refuses at once, the commit of a view
// lent before included.
func TestWritesAreRefusedAfterCloseWrite(t *testing.T) {
	l := &recorder{}
	s := rsproto.NewStream(l, testConfig())
	s.Connect(testPeer)
	s.Reserve()
	s.CloseWrite()
	l.posts = nil

	if s.CanSend() || s.Reserve() != nil {
		t.Errorf("CanSend() = %v and Reserve() lent %d bytes after CloseWrite, want false and nothing", s.CanSend(), len(s.Reserve()))
	}
	if err := s.Commit(1); err == nil || len(l.posts) != 0 {
		t.Errorf("Commit(1) of a view lent before CloseWrite = %v and posted %+v, want an error and nothing", err, l.posts)
	}
}

// rs_poll_cq takes the peer's RS_CTRL_SHUTDOWN, while the rsocket is still
// writable, by clearing rs_readable alone: rrecv returns what arrived before
// and then 0, and rsend goes on.
func TestPeerShutdownEndsReadsBehindTheDataAndWritesGoOn(t *testing.T) {
	c := testConfig()
	l := &recorder{}
	s := rsproto.NewStream(l, c)
	s.Connect(testPeer)
	peerWrites(s, c, 0, []byte("before"))
	l.recvs = nil
	s.Received(rsproto.MakeMsg(rsproto.OpCtrl, rsproto.CtrlShutdown))

	if want := rsproto.StateConnected | rsproto.StateWritable; s.State() != want || s.EOF() || string(s.Peek()) != "before" || len(l.recvs) != 1 {
		t.Fatalf("after the peer's shutdown the state is %#x, EOF() = %v with %q lent and %d receives posted; want %#x, false with %q and 1", s.State(), s.EOF(), s.Peek(), len(l.recvs), want, "before")
	}
	s.Consume(len("before"))
	if !s.EOF() {
		t.Errorf("EOF() = false once the data before the peer's shutdown is read")
	}
	l.posts = nil
	if v := s.Reserve(); len(v) == 0 || s.Commit(1) != nil || len(l.data()) != 1 {
		t.Errorf("after the peer's shutdown Reserve() lent %d bytes and Commit(1) posted %+v, want room and a data message", len(v), l.data())
	}
}

// rshutdown waits for rs_ctrl_avail before it posts its control message,
// which takes a slot of ctrl_seqno as grants and credits do; the stream holds
// the message and posts it at the first slot a completion gives back, ahead
// of the credits held for one too.
func TestShutdownWaitsForAControlSlot(t *testing.T) {
	s, l := fourCreditsDue(t)
	s.CloseWrite()
	if len(l.posts) != rsproto.QPCtrlSize {
		t.Fatalf("CloseWrite with %d grants and credits in flight posted %+v, want nothing", rsproto.QPCtrlSize, l.posts[rsproto.QPCtrlSize:])
	}
	s.Sent(l.posts[0].id, nil)
	if len(l.posts) != rsproto.QPCtrlSize+1 || !isCtrl(l.posts[rsproto.QPCtrlSize], rsproto.CtrlShutdown) {
		t.Errorf("the completion of the grant of Connect posted %+v, want RS_CTRL_SHUTDOWN alone", l.posts[rsproto.QPCtrlSize:])
	}
}

// rs_poll_cq gives a control slot back, ctrl_max_seqno++, when an RS_OP_CTRL
// send completes as when an RS_OP_SGL one does, and rs_process_cq then sends
// the credits held for it.
func TestControlCompletionGivesBackItsSlot(t *testing.T) {
	c := testConfig()
	c.ReceiveQueueSize = rsproto.QPMinSize
	l := &recorder{}
	s := rsproto.NewStream(l, c)
	s.Connect(testPeer)
	s.CloseWrite()
	read := (rsproto.QPCtrlSize - 1) * rsproto.QPMinSize / 2
	readMessages(s, c, 0, read)
	if len(l.posts) != rsproto.QPCtrlSize || !isCtrl(l.posts[1], rsproto.CtrlShutdown) {
		t.Fatalf("Connect, CloseWrite and reading %d messages posted %+v, want a grant, RS_CTRL_SHUTDOWN and two credits", read, l.posts)
	}

	s.Sent(l.posts[1].id, nil)
	if want := uint32(read + rsproto.QPMinSize); len(l.posts) != rsproto.QPCtrlSize+1 || !isCredit(l.posts[rsproto.QPCtrlSize], want) {
		t.Errorf("the completion of RS_CTRL_SHUTDOWN posted %+v, want the credits for %d held for its slot", l.posts[rsproto.QPCtrlSize:], want)
	}
}

// rshutdown(SHUT_RDWR), which rclose makes, clears rs_readable and
// rs_writable of a connected rsocket and posts RS_CTRL_DISCONNECT.
func TestDisconnectSendsDisconnect(t *testing.T) {
	l := &recorder{}
	s := rsproto.NewStream(l, testConfig())
	s.Connect(testPeer)
	l.posts = nil

	s.Disconnect()
	if len(l.posts) != 1 || !isCtrl(l.posts[0], rsproto.CtrlDisconnect) || s.State() != rsproto.StateConnected {
		t.Errorf("Disconnect posted %+v and left the state %#x, want RS_CTRL_DISCONNECT and %#x", l.posts, s.State(), rsproto.StateConnected)
	}
}

// rclose after rshutdown(SHUT_WR) posts RS_CTRL_DISCONNECT after the
// RS_CTRL_SHUTDOWN, each once rs_ctrl_avail: the stream holds both and posts
// them in that order as completions give slots back.
func TestHeldShutdownAndDisconnectGoOutInOrder(t *testing.T) {
	s, l := fourCreditsDue(t)
	s.CloseWrite()
	s.Disconnect()
	if len(l.posts) != rsproto.QPCtrlSize {
		t.Fatalf("CloseWrite and Disconnect with %d grants and credits in flight posted %+v, want nothing", rsproto.QPCtrlSize, l.posts[rsproto.QPCtrlSize:])
	}
	s.Sent(l.posts[0].id, nil)
	s.Sent(l.posts[1].id, nil)
	if got := l.posts[rsproto.QPCtrlSize:]; len(got) != 2 || !isCtrl(got[0], rsproto.CtrlShutdown) || !isCtrl(got[1], rsproto.CtrlDisconnect) {
		t.Errorf("two completions posted %+v, want RS_CTRL_SHUTDOWN and RS_CTRL_DISCONNECT", got)
	}
}

// rs_poll_cq turns a failed send into rs_error with EIO only while the
// rsocket is rs_connected, so the sends flushed after an orderly end of the
// connection are no errors, and the completion of its own RS_CTRL_DISCONNECT
// ends it, failed or not.
func TestCompletionFlushedAfterAnOrderlyEndIsNoError(t *testing.T) {
	flushed := errors.New("IBV_WC_WR_FLUSH_ERR")
	tests := []struct {
		c   string // the C function and case that ends the connection
		end func(s *rsproto.Stream, l *recorder)
	}{
		{"rs_poll_cq of the completion of RS_CTRL_DISCONNECT", func(s *rsproto.Stream, l *recorder) {
			s.Disconnect()
			s.Sent(l.posts[len(l.posts)-1].id, nil)
		}},
		{"rs_poll_cq of RS_CTRL_DISCONNECT flushed itself", func(s *rsproto.Stream, l *recorder) {
			s.Disconnect()
			s.Sent(l.posts[len(l.posts)-1].id, flushed)
		}},
		{"rs_poll_cq of the peer's RS_CTRL_DISCONNECT", func(s *rsproto.Stream, l *recorder) {
			s.Received(rsproto.MakeMsg(rsproto.OpCtrl, rsproto.CtrlDisconnect))
		}},
		{"rs_poll_cq of the peer's RS_CTRL_SHUTDOWN after rshutdown(SHUT_WR)", func(s *rsproto.Stream, l *recorder) {
			s.CloseWrite()
			s.Received(rsproto.MakeMsg(rsproto.OpCtrl, rsproto.CtrlShutdown))
		}},
	}
	for _, tt := range tests {
		t.Run(tt.c, func(t *testing.T) {
			l := &recorder{}
			s := rsproto.NewStream(l, testConfig())
			s.Connect(testPeer)
			sendWhileCan(t, s, 1)
			data := l.data()[0]
			tt.end(s, l)
			s.Sent(data.id, flushed)

			if err := s.Err(); s.State() != rsproto.StateDisconnected || !errors.Is(err, io.EOF) {
				t.Errorf("a send flushed after the end left the state %#x and Err() = %v, want %#x and io.EOF", s.State(), err, rsproto.StateDisconnected)
			}
		})
	}
}

// rs_poll_cq takes the peer's RS_CTRL_DISCONNECT by making the rsocket
// rs_disconnected, returning before it posts a receive in place of the one
// used up; rrecv still returns what arrived before, then 0, and rsend fails.
func TestPeerDisconnectEndsTheStream(t *testing.T) {
	c := testConfig()
	l := &recorder{}
	s := rsproto.NewStream(l, c)
	s.Connect(testPeer)
	peerWrites(s, c, 0, []byte("before"))
	l.recvs = nil
	s.Received(rsproto.MakeMsg(rsproto.OpCtrl, rsproto.CtrlDisconnect))

	if err := s.Err(); s.State() != rsproto.StateDisconnected || !errors.Is(err, io.EOF) || len(l.recvs) != 0 {
		t.Fatalf("the peer's disconnect left the state %#x and Err() = %v, and posted %d receives; want %#x, io.EOF and none", s.State(), err, len(l.recvs), rsproto.StateDisconnected)
	}
	if s.EOF() || string(s.Peek()) != "before" {
		t.Fatalf("EOF() = %v with %q lent after the peer's disconnect, want false and %q", s.EOF(), s.Peek(), "before")
	}
	s.Consume(len("before"))
	if !s.EOF() || s.Reserve() != nil {
		t.Errorf("EOF() = %v and Reserve() lent %d bytes once the data before the peer's disconnect is read, want true and nothing", s.EOF(), len(s.Reserve()))
	}
}

// rs_poll_cq takes the peer's RS_CTRL_SHUTDOWN once rs_writable is clear as
// the end of both sides: the rsocket is rs_disconnected and posts no receive
// in place of the one used up.
func TestPeerShutdownAfterCloseWriteEndsTheStream(t *testing.T) {
	l := &recorder{}
	s := rsproto.NewStream(l, testConfig())
	s.Connect(testPeer)
	s.CloseWrite()
	l.recvs = nil
	s.Received(rsproto.MakeMsg(rsproto.OpCtrl, rsproto.CtrlShutdown))

	if err := s.Err(); s.State() != rsproto.StateDisconnected || !errors.Is(err, io.EOF) || len(l.recvs) != 0 {
		t.Errorf("the peer's shutdown after CloseWrite left the state %#x and Err() = %v, and posted %d receives; want %#x, io.EOF and none", s.State(), err, len(l.recvs), rsproto.StateDisconnected)
	}
}

// rshutdown(SHUT_WR) of a connected rsocket whose rs_readable the peer's
// shutdown has cleared posts RS_CTRL_DISCONNECT in place of RS_CTRL_SHUTDOWN.
func TestCloseWriteAfterPeerShutdownSendsDisconnect(t *testing.T) {
	l := &recorder{}
	s := rsproto.NewStream(l, testConfig())
	s.Connect(testPeer)
	s.Received(rsproto.MakeMsg(rsproto.OpCtrl, rsproto.CtrlShutdown))
	l.posts = nil

	s.CloseWrite()
	if len(l.posts) != 1 || !isCtrl(l.posts[0], rsproto.CtrlDisconnect) || s.State() != rsproto.StateConnected {
		t.Errorf("CloseWrite after the peer's shutdown posted %+v and left the state %#x, want RS_CTRL_DISCONNECT and %#x", l.posts, s.State(), rsproto.StateConnected)
	}
}

// rshutdown posts nothing unless the rsocket is rs_connected, neither at once
// nor once a control slot it waited for is free.
func TestNoControlMessageOnceTheStreamEnds(t *testing.T) {
	end := rsproto.MakeMsg(rsproto.OpCtrl, rsproto.CtrlDisconnect)
	tests := []struct {
		c     string // the C function and case that posts nothing
		close func(s *rsproto.Stream)
	}{
		{"rshutdown(SHUT_WR) of an rs_disconnected rsocket", func(s *rsproto.Stream) { s.Received(end); s.CloseWrite() }},
		{"rshutdown(SHUT_RDWR) of an rs_disconnected rsocket", func(s *rsproto.Stream) { s.Received(end); s.Disconnect() }},
		{"rshutdown(SHUT_WR) waiting for a control slot as the peer disconnects", func(s *rsproto.Stream) { s.CloseWrite(); s.Received(end) }},
	}
	for _, tt := range tests {
		t.Run(tt.c, func(t *testing.T) {
			s, l := fourCreditsDue(t)
			tt.close(s)
			for _, p := range l.posts[:rsproto.QPCtrlSize] {
				s.Sent(p.id, nil)
			}
			if got := l.posts[rsproto.QPCtrlSize:]; len(got) != 0 {
				t.Errorf("the control slots coming back posted %+v, want nothing", got)
			}
		})
	}
}

// rshutdown drops the control message it waits a slot for once the peer's
// RS_CTRL_SHUTDOWN makes the rsocket rs_disconnected, which leaves the peer
// to learn the end from the loss of the link; the stream posts it at the next
// free slot, and the peer, which has shut down writing, takes either message
// as the end in rs_poll_cq.
func TestHeldEndGoesOutAfterThePeerShutdown(t *testing.T) {
	tests := []struct {
		c    string // the control message and the C call that holds it
		end  func(s *rsproto.Stream)
		ctrl uint32
	}{
		{"RS_CTRL_SHUTDOWN of rshutdown(SHUT_WR)", (*rsproto.Stream).CloseWrite, rsproto.CtrlShutdown},
		{"RS_CTRL_DISCONNECT of rshutdown(SHUT_RDWR)", (*rsproto.Stream).Disconnect, rsproto.CtrlDisconnect},
	}
	for _, tt := range tests {
		t.Run(tt.c, func(t *testing.T) {
			s, l := fourCreditsDue(t)
			tt.end(s)
			s.Received(rsproto.MakeMsg(rsproto.OpCtrl, rsproto.CtrlShutdown))
			for _, p := range l.posts[:rsproto.QPCtrlSize] {
				s.Sent(p.id, nil)
			}
			if got := l.posts[rsproto.QPCtrlSize:]; len(got) != 1 || !isCtrl(got[0], tt.ctrl) {
				t.Errorf("the control slots coming back after the peer's shutdown posted %+v, want the %s held", got, tt.c)
			}
		})
	}
}

// rs_conn_all_sends_done, which rshutdown waits for before it takes the link
// down, holds as soon as the peer's RS_CTRL_SHUTDOWN makes the rsocket
// rs_disconnected; the stream waits for the end it held to go out and
// complete too, so that the link outlives it.
func TestSendsDoneWaitsForTheEndHeldAtThePeerShutdown(t *testing.T) {
	s, l := fourCreditsDue(t)
	s.CloseWrite()
	s.Received(rsproto.MakeMsg(rsproto.OpCtrl, rsproto.CtrlShutdown))
	for i, p := range l.posts[:rsproto.QPCtrlSize] {
		if s.SendsDone() {
			t.Fatalf("SendsDone() after the peer's shutdown with %d of the %d credits completed and RS_CTRL_SHUTDOWN held", i, rsproto.QPCtrlSize)
		}
		s.Sent(p.id, nil)
	}
	if len(l.posts) != rsproto.QPCtrlSize+1 || s.SendsDone() {
		t.Fatalf("the control slots coming back posted %+v, and SendsDone() = %v; want RS_CTRL_SHUTDOWN in flight and false", l.posts[rsproto.QPCtrlSize:], s.SendsDone())
	}
	s.Sent(l.posts[rsproto.QPCtrlSize].id, nil)
	if !s.SendsDone() {
		t.Error("SendsDone() = false once RS_CTRL_SHUTDOWN, the last post, completed")
	}
}

// rs_give_credits gives none unless the rsocket is rs_connected, which a
// failed send, rs_error, and a disconnect, rs_disconnected, end.
func TestNoCreditsAfterTheEnd(t *testing.T) {
	tests := []struct {
		c   string // the C function and case that ends the connection
		end func(s *rsproto.Stream, l *recorder)
	}{
		{"rs_poll_cq of a failed send", func(s *rsproto.Stream, l *recorder) { s.Sent(l.data()[0].id, errors.New("IBV_WC_RETRY_EXC_ERR")) }},
		{"rs_poll_cq of the peer's RS_CTRL_DISCONNECT", func(s *rsproto.Stream, l *recorder) {
			s.Received(rsproto.MakeMsg(rsproto.OpCtrl, rsproto.CtrlDisconnect))
		}},
		{"rs_handle_cm_event of RDMA_CM_EVENT_DISCONNECTED", func(s *rsproto.Stream, l *recorder) {
			s.LinkDown(errors.New("RDMA_CM_EVENT_DISCONNECTED"))
		}},
	}
	for _, tt := range tests {
		t.Run(tt.c, func(t *testing.T) {
			c := testConfig()
			l := &recorder{}
			s := rsproto.NewStream(l, c)
			s.Connect(testPeer)
			sendWhileCan(t, s, 1)
			half := len(c.ReceiveBuffer.Buf) / 2
			peerWrites(s, c, 0, make([]byte, half))
			tt.end(s, l)
			l.posts = nil

			s.Peek()
			s.Consume(half)
			if len(l.posts) != 0 {
				t.Errorf("reading half the receive buffer after the end posted %+v, want nothing", l.posts)
			}
		})
	}
}

// rs_poll_cq makes even an rs_error rsocket rs_disconnected when its own
// RS_CTRL_DISCONNECT completes or the peer's arrives, but err keeps the EIO
// of the failure: the end is no orderly one.
func TestFailureStaysTheCauseOfTheEnd(t *testing.T) {
	tests := []struct {
		c   string // the C function and case that ends the connection
		end func(s *rsproto.Stream, l *recorder)
	}{
		{"rs_poll_cq of the completion of RS_CTRL_DISCONNECT, flushed", func(s *rsproto.Stream, l *recorder) {
			s.Sent(l.posts[len(l.posts)-1].id, errors.New("IBV_WC_WR_FLUSH_ERR"))
		}},
		{"rs_poll_cq of the peer's RS_CTRL_DISCONNECT", func(s *rsproto.Stream, l *recorder) {
			s.Received(rsproto.MakeMsg(rsproto.OpCtrl, rsproto.CtrlDisconnect))
		}},
	}
	for _, tt := range tests {
		t.Run(tt.c, func(t *testing.T) {
			l := &recorder{}
			s := rsproto.NewStream(l, testConfig())
			s.Connect(testPeer)
			sendWhileCan(t, s, 1)
			s.Disconnect()
			failed := errors.New("IBV_WC_RETRY_EXC_ERR")
			s.Sent(l.data()[0].id, failed)
			tt.end(s, l)

			if err := s.Err(); s.State() != rsproto.StateDisconnected || !errors.Is(err, failed) {
				t.Errorf("the end after a failed send left the state %#x and Err() = %v, want %#x and an error wrapping %q", s.State(), err, rsproto.StateDisconnected, failed)
			}
		})
	}
}

// rs_handle_cm_event makes a connected rsocket rs_disconnected at
// RDMA_CM_EVENT_DISCONNECTED, err 0 as for an orderly end; the stream keeps
// the state but has the loss for its cause, not io.EOF.
func TestLinkLossWhileConnectedIsAnError(t *testing.T) {
	s := rsproto.NewStream(&recorder{}, testConfig())
	s.Connect(testPeer)
	lost := errors.New("RDMA_CM_EVENT_DISCONNECTED")
	s.LinkDown(lost)

	if err := s.Err(); s.State() != rsproto.StateDisconnected || !errors.Is(err, lost) || errors.Is(err, io.EOF) {
		t.Errorf("the link lost left the state %#x and Err() = %v, want %#x and an error wrapping %q, not the io.EOF of an orderly end", s.State(), err, rsproto.StateDisconnected, lost)
	}
}

// rs_handle_cm_event takes RDMA_CM_EVENT_DISCONNECTED only while the rsocket
// is rs_connected: after an orderly end or a failure, state and cause stay.
func TestLinkLossAfterTheEndChangesNothing(t *testing.T) {
	tests := []struct {
		c   string // the C function and case that ended the connection
		end func(s *rsproto.Stream, l *recorder)
	}{
		{"rs_poll_cq of the peer's RS_CTRL_DISCONNECT", func(s *rsproto.Stream, l *recorder) {
			s.Received(rsproto.MakeMsg(rsproto.OpCtrl, rsproto.CtrlDisconnect))
		}},
		{"rs_poll_cq of a failed send", func(s *rsproto.Stream, l *recorder) { s.Sent(l.data()[0].id, errors.New("IBV_WC_RETRY_EXC_ERR")) }},
	}
	for _, tt := range tests {
		t.Run(tt.c, func(t *testing.T) {
			l := &recorder{}
			s := rsproto.NewStream(l, testConfig())
			s.Connect(testPeer)
			sendWhileCan(t, s, 1)
			tt.end(s, l)
			state, err := s.State(), s.Err()
			s.LinkDown(errors.New("RDMA_CM_EVENT_DISCONNECTED"))

			if s.State() != state || s.Err() != err {
				t.Errorf("the link lost after the end changed the state %#x and Err() = %v to %#x and %v", state, err, s.State(), s.Err())
			}
		})
	}
}

// rs_conn_have_rdata, which rrecv waits for, holds once data has arrived or
// rs_readable is clear.
func TestReadableWithDataOrTheEnd(t *testing.T) {
	c := testConfig()
	s := rsproto.NewStream(&recorder{}, c)
	s.Connect(testPeer)
	if s.Readable() {
		t.Fatal("Readable() with nothing arrived")
	}
	peerWrites(s, c, 0, []byte("x"))
	if !s.Readable() {
		t.Fatal("Readable() = false with a byte arrived")
	}
	s.Peek()
	s.Consume(1)
	if s.Readable() {
		t.Fatal("Readable() once all that arrived is read")
	}
	s.Received(rsproto.MakeMsg(rsproto.OpCtrl, rsproto.CtrlShutdown))
	if !s.Readable() {
		t.Error("Readable() = false after the peer's shutdown")
	}
}

// rs_conn_can_send, which rsend waits for, holds once rs_can_send does or
// rs_writable is clear, when rsend fails.
func TestWritableWithRoomOrTheEnd(t *testing.T) {
	peer := testPeer
	peer.Credits = 1
	s := rsproto.NewStream(&recorder{}, testConfig())
	s.Connect(peer)
	if !s.Writable() {
		t.Fatal("Writable() = false with room and a credit")
	}
	sendWhileCan(t, s, 1)
	if s.Writable() {
		t.Fatal("Writable() with no credit left")
	}
	s.CloseWrite()
	if !s.Writable() {
		t.Error("Writable() = false after CloseWrite")
	}
}

// rs_conn_all_sends_done, which rshutdown waits for, holds once every send
// queue entry and control slot is back, or the rsocket is no longer
// rs_connected.
func TestSendsDoneOnceAllCompleteOrTheEnd(t *testing.T) {
	l := &recorder{}
	s := rsproto.NewStream(l, testConfig())
	s.Connect(testPeer)
	sendWhileCan(t, s, 1)
	for i, p := range l.posts {
		if s.SendsDone() {
			t.Fatalf("SendsDone() with %d of the grant and the data message completed", i)
		}
		s.Sent(p.id, nil)
	}
	if !s.SendsDone() {
		t.Fatal("SendsDone() = false with the grant and the data message completed")
	}
	sendWhileCan(t, s, 1)
	s.Received(rsproto.MakeMsg(rsproto.OpCtrl, rsproto.CtrlDisconnect))
	if !s.SendsDone() {
		t.Error("SendsDone() = false with a data message in flight after the peer's disconnect")
	}
}

// rshutdown returns the error of the rs_post_msg it could not make, and
// rs_poll_cq turns a receive it cannot post into rs_error; the stream breaks
// at a control message refused too, as at any post it makes on its own.
func TestRefusedControlBreaksTheStream(t *testing.T) {
	refused := errors.New("ibv_post_send: EINVAL")
	l := &recorder{}
	s := rsproto.NewStream(l, testConfig())
	s.Connect(testPeer)
	l.err = refused
	s.CloseWrite()

	if err := s.Err(); s.State() != rsproto.StateError || !errors.Is(err, refused) {
		t.Errorf("CloseWrite with the post refused left the state %#x and Err() = %v, want %#x and an error wrapping %q", s.State(), err, rsproto.StateError, refused)
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
