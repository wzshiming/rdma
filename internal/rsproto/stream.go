package rsproto

import (
	"encoding/binary"
	"fmt"
)

// Region is a block of local memory registered for RDMA: its bytes, and the
// address and key the link and the peer know it by.
type Region struct {
	Buf  []byte
	Addr uint64
	Key  uint32
}

// Config is what a Stream is built from besides its peer.
type Config struct {
	SendBuffer    Region // Key is the local key the link reads it by
	ReceiveBuffer Region // the peer writes it; Key is the remote key
	GrantTable    Region // SGLSize entries of SGESize bytes the peer writes; Key is the remote key

	// The depths of the queue pair's queues, as rs_set_qp_size leaves them
	// within QPMinSize and QPMaxSize.
	SendQueueSize    int
	ReceiveQueueSize int

	BigEndian bool // the host is big-endian
}

// Stream is the state machine of one rsockets stream connection. It does no
// I/O but through its Link, and its caller drives it one call at a time.
type Stream struct {
	link  Link
	c     Config
	order binary.ByteOrder // of the host, which the grant table is in

	targetSGE int        // the entry of the grant table data goes to
	sqeAvail  int        // send queue entries free for data
	sbufAvail int        // bytes of the send buffer free
	sbufPos   int        // where the free bytes of the send buffer start: ssgl[0].addr
	sseqNo    uint16     // data messages sent, wrapping
	sseqComp  uint16     // the sseqNo the peer's credits stop sending at
	sgl       [1]Segment // of a post, here so that passing it allocates nothing
	err       error
}

// recvID is the id of every receive, rs_recv_wr_id(0).
const recvID = 1 << 63

// NewStream returns a stream on memory the caller registered, as
// rs_create_ep leaves it for a connection to be set up: with
// ReceiveQueueSize receives posted.
func NewStream(l Link, c Config) *Stream {
	clear(c.GrantTable.Buf[:SGLSize*SGESize])
	s := &Stream{
		link:      l,
		c:         c,
		order:     ConnData{BigEndian: c.BigEndian}.ByteOrder(),
		sqeAvail:  c.SendQueueSize - QPCtrlSize,
		sbufAvail: len(c.SendBuffer.Buf),
	}
	for range c.ReceiveQueueSize {
		l.PostRecv(recvID)
	}
	return s
}

// ConnData mirrors rs_format_conn_data: the connection data to send the peer.
func (s *Stream) ConnData() ConnData {
	return ConnData{
		BigEndian:  s.c.BigEndian,
		Credits:    uint16(s.c.ReceiveQueueSize),
		GrantTable: SGE{Addr: s.c.GrantTable.Addr, Key: s.c.GrantTable.Key, Length: SGLSize},
		FirstGrant: SGE{Addr: s.c.ReceiveBuffer.Addr, Key: s.c.ReceiveBuffer.Key, Length: uint32(len(s.c.ReceiveBuffer.Buf) / 2)},
	}
}

// Connect mirrors rs_save_conn_data: the stream takes the connection data of
// its peer.
func (s *Stream) Connect(peer ConnData) {
	s.setGrant(0, peer.FirstGrant)
	s.sseqComp = peer.Credits
}

// grant returns entry i of the grant table, target_sgl[i] in C, which the
// peer writes and the stream uses up.
func (s *Stream) grant(i int) SGE {
	return GetSGE(s.c.GrantTable.Buf[i*SGESize:], s.order)
}

func (s *Stream) setGrant(i int, g SGE) {
	PutSGE(s.c.GrantTable.Buf[i*SGESize:], s.order, g)
}

// CanSend mirrors rs_can_send: whether Reserve would lend room now.
func (s *Stream) CanSend() bool {
	return s.sqeAvail > 0 && s.sbufAvail >= SndLowat && s.sseqNo != s.sseqComp && s.grant(s.targetSGE).Length != 0
}

// Reserve lends the room of the send buffer that one message can carry now,
// nil when there is none.
func (s *Stream) Reserve() []byte {
	if !s.CanSend() {
		return nil
	}
	n := min(s.sbufAvail, len(s.c.SendBuffer.Buf)-s.sbufPos, int(s.grant(s.targetSGE).Length), MaxTransfer)
	return s.c.SendBuffer.Buf[s.sbufPos : s.sbufPos+n : s.sbufPos+n]
}

// Commit ends the reservation, sending the first n bytes of the view as one
// data message, as rs_write_data does.
func (s *Stream) Commit(n int) error {
	if n == 0 {
		return nil
	}
	s.sseqNo++
	s.sqeAvail--
	s.sbufAvail -= n
	g := s.grant(s.targetSGE)
	addr, key := g.Addr, g.Key
	g.Addr += uint64(n)
	g.Length -= uint32(n)
	s.setGrant(s.targetSGE, g)
	if g.Length == 0 {
		s.targetSGE = (s.targetSGE + 1) % SGLSize
	}

	s.sgl[0] = Segment{Addr: s.c.SendBuffer.Addr + uint64(s.sbufPos), Length: uint32(n), Key: s.c.SendBuffer.Key}
	if s.sbufPos += n; s.sbufPos == len(s.c.SendBuffer.Buf) {
		s.sbufPos = 0
	}
	msg := MakeMsg(OpData, uint32(n))
	return s.link.PostWriteMsg(uint64(msg), s.sgl[:], msg, addr, key)
}

// Sent takes the completion of the post with the given id, as rs_poll_cq
// does.
func (s *Stream) Sent(id uint64, err error) {
	s.sqeAvail++
	s.sbufAvail += int(Msg(id).Data())
	if err != nil && s.err == nil {
		s.err = fmt.Errorf("rsproto: send failed: %w", err)
	}
}

// Received takes a message word the peer sent, as rs_poll_cq does.
func (s *Stream) Received(msg Msg) {
	if msg.Op() == OpSGL {
		s.sseqComp = uint16(msg.Data())
	}
}

// Err returns why the stream broke, nil while it has not.
func (s *Stream) Err() error { return s.err }
