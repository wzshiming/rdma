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
	ControlBuffer Region // QPCtrlSize slots of SGESize bytes grants are posted from, rs_get_ctrl_buf's; Key is the local key

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

	rbufOffset   int              // where the arrived bytes start in the receive buffer: rbuf_offset
	rbufArrived  int              // bytes arrived and not yet consumed
	rmsg         []uint32         // a ring of what is left of the data messages not read whole
	rmsgHead     int              // the oldest of them
	rmsgTail     int              // where the next goes
	rbufAvail    int              // bytes of the receive buffer free and not granted: rbuf_bytes_avail
	rbufFree     int              // where the next grant starts: rbuf_free_offset
	remote       SGE              // the peer's grant table, Length counting entries: remote_sgl
	remoteSGE    int              // the entry of it the next grant goes to
	remoteOrder  binary.ByteOrder // of the peer's host, which its grant table is in
	ctrlSeqNo    uint32           // credit messages posted: ctrl_seqno
	ctrlMaxSeqNo uint32           // the ctrlSeqNo the slots for them run out at
	connected    bool             // Connect has taken the peer's connection data
	rseqNo       uint16           // data messages read, wrapping
	rseqComp     uint16           // the rseqNo to give credits at

	err error
}

// recvID is the id of every receive, rs_recv_wr_id(0).
const recvID = 1 << 63

// NewStream returns a stream on memory the caller registered, as
// rs_create_ep leaves it for a connection to be set up: with
// ReceiveQueueSize receives posted. A receive the link refuses breaks it.
func NewStream(l Link, c Config) *Stream {
	clear(c.GrantTable.Buf[:SGLSize*SGESize])
	s := &Stream{
		link:         l,
		c:            c,
		order:        ConnData{BigEndian: c.BigEndian}.ByteOrder(),
		sqeAvail:     c.SendQueueSize - QPCtrlSize,
		sbufAvail:    len(c.SendBuffer.Buf),
		rbufAvail:    len(c.ReceiveBuffer.Buf) / 2,
		rbufFree:     len(c.ReceiveBuffer.Buf) / 2,
		rmsg:         make([]uint32, c.ReceiveQueueSize+1),
		rseqComp:     uint16(c.ReceiveQueueSize / 2),
		ctrlMaxSeqNo: QPCtrlSize,
	}
	for range c.ReceiveQueueSize {
		if err := l.PostRecv(recvID); err != nil {
			s.err = fmt.Errorf("rsproto: post failed: %w", err)
			break
		}
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
// its peer. It grants the peer room as the first rs_process_cq of the
// connection does, so the link must be able to send.
func (s *Stream) Connect(peer ConnData) {
	s.setGrant(0, peer.FirstGrant)
	s.sseqComp = peer.Credits
	s.remote = peer.GrantTable
	s.remoteSGE = 1
	s.remoteOrder = peer.ByteOrder()
	s.connected = true
	s.updateCredits()
}

// updateCredits mirrors rs_update_credits, which rs_give_credits decides.
func (s *Stream) updateCredits() {
	if (s.rbufAvail >= len(s.c.ReceiveBuffer.Buf)/2 || int16(s.rseqNo-s.rseqComp) >= 0) && s.ctrlSeqNo != s.ctrlMaxSeqNo && s.connected {
		s.sendCredits()
	}
}

// sendCredits mirrors rs_send_credits.
func (s *Stream) sendCredits() {
	s.ctrlSeqNo++
	s.rseqComp = s.rseqNo + uint16(s.c.ReceiveQueueSize/2)
	msg := MakeMsg(OpSGL, uint32(s.rseqNo)+uint32(s.c.ReceiveQueueSize))
	half := len(s.c.ReceiveBuffer.Buf) / 2
	if s.rbufAvail < half {
		if err := s.link.PostMsg(uint64(msg), msg); err != nil && s.err == nil {
			s.err = fmt.Errorf("rsproto: post failed: %w", err)
		}
		return
	}
	ctrl := s.c.ControlBuffer
	slot := int(s.ctrlSeqNo%QPCtrlSize) * SGESize
	PutSGE(ctrl.Buf[slot:], s.remoteOrder, SGE{Addr: s.c.ReceiveBuffer.Addr + uint64(s.rbufFree), Key: s.c.ReceiveBuffer.Key, Length: uint32(half)})
	s.sgl[0] = Segment{Addr: ctrl.Addr + uint64(slot), Length: SGESize, Key: ctrl.Key}
	if err := s.link.PostWriteMsg(uint64(msg), s.sgl[:], msg, s.remote.Addr+uint64(s.remoteSGE)*SGESize, s.remote.Key); err != nil && s.err == nil {
		s.err = fmt.Errorf("rsproto: post failed: %w", err)
	}
	s.rbufAvail -= half
	if s.rbufFree += half; s.rbufFree >= len(s.c.ReceiveBuffer.Buf) {
		s.rbufFree = 0
	}
	if s.remoteSGE++; s.remoteSGE == int(s.remote.Length) {
		s.remoteSGE = 0
	}
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
// does, and gives the peer the credits held for a control slot.
func (s *Stream) Sent(id uint64, err error) {
	if Msg(id).Op() == OpSGL {
		s.ctrlMaxSeqNo++
	} else {
		s.sqeAvail++
		s.sbufAvail += int(Msg(id).Data())
	}
	if err != nil && s.err == nil {
		s.err = fmt.Errorf("rsproto: send failed: %w", err)
	}
	s.updateCredits()
}

// Received takes a message word the peer sent, as rs_poll_cq does, and posts
// a receive in place of the one it used up.
func (s *Stream) Received(msg Msg) {
	switch msg.Op() {
	case OpSGL:
		s.sseqComp = uint16(msg.Data())
	case OpWrite, OpIOMapSGL, OpCtrl: // no data
	default:
		s.rmsg[s.rmsgTail] = msg.Data()
		if s.rmsgTail++; s.rmsgTail == len(s.rmsg) {
			s.rmsgTail = 0
		}
		s.rbufArrived += int(msg.Data())
	}
	if err := s.link.PostRecv(recvID); err != nil && s.err == nil {
		s.err = fmt.Errorf("rsproto: post failed: %w", err)
	}
}

// Peek lends the bytes that have arrived and lie contiguous in the receive
// buffer, in the order they arrived: what rs_peek copies, without the copy.
func (s *Stream) Peek() []byte {
	rbuf := s.c.ReceiveBuffer.Buf
	end := s.rbufOffset + min(s.rbufArrived, len(rbuf)-s.rbufOffset)
	return rbuf[s.rbufOffset:end:end]
}

// Consume ends the loan of Peek with the first n bytes of the view read, as
// rrecv takes them, and gives the peer credits as rs_update_credits does.
func (s *Stream) Consume(n int) {
	s.rbufArrived -= n
	s.rbufAvail += n
	if s.rbufOffset += n; s.rbufOffset == len(s.c.ReceiveBuffer.Buf) {
		s.rbufOffset = 0
	}
	for n > 0 {
		head := &s.rmsg[s.rmsgHead]
		if n < int(*head) {
			*head -= uint32(n)
			break
		}
		n -= int(*head)
		s.rseqNo++
		if s.rmsgHead++; s.rmsgHead == len(s.rmsg) {
			s.rmsgHead = 0
		}
	}
	s.updateCredits()
}

// Err returns why the stream broke, nil while it has not.
func (s *Stream) Err() error { return s.err }
