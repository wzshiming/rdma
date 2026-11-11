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
