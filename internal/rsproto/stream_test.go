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
