package rsproto

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
	link Link
	c    Config

	target  SGE // the peer's grant data goes to: target_sgl[target_sge]
	sbufPos int // where the free bytes of the send buffer start: ssgl[0].addr
	sgl     [2]Segment
}

// NewStream returns a stream on memory the caller registered, as
// rs_create_ep leaves it for a connection to be set up.
func NewStream(l Link, c Config) *Stream {
	return &Stream{link: l, c: c}
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
	s.target = peer.FirstGrant
}

// CanSend mirrors rs_can_send: whether Reserve would lend room now.
func (s *Stream) CanSend() bool {
	return s.target.Length != 0
}

// Reserve lends the room of the send buffer that one message can carry now,
// nil when there is none.
func (s *Stream) Reserve() []byte {
	if !s.CanSend() {
		return nil
	}
	n := min(len(s.c.SendBuffer.Buf)-s.sbufPos, int(s.target.Length))
	return s.c.SendBuffer.Buf[s.sbufPos : s.sbufPos+n : s.sbufPos+n]
}

// Commit ends the reservation, sending the first n bytes of the view as one
// data message, as rs_write_data does.
func (s *Stream) Commit(n int) error {
	addr, key := s.target.Addr, s.target.Key
	s.target.Addr += uint64(n)
	s.target.Length -= uint32(n)

	s.sgl[0] = Segment{Addr: s.c.SendBuffer.Addr + uint64(s.sbufPos), Length: uint32(n), Key: s.c.SendBuffer.Key}
	s.sbufPos += n
	msg := MakeMsg(OpData, uint32(n))
	s.link.PostWriteMsg(0, s.sgl[:1], msg, addr, key)
	return nil
}
