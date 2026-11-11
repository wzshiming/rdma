package rsproto

// Segment is local memory a post reads, as struct ibv_sge describes it.
type Segment struct {
	Addr   uint64
	Length uint32
	Key    uint32 // the local key
}

// Link is all a Stream asks of the transport under it, in the IB/RoCE mode
// of rsockets. Each post carries an id of the stream's choosing, by which the
// caller reports the completion of the post back to the stream.
type Link interface {
	// PostWriteMsg mirrors rs_post_write_msg: an RDMA write with immediate
	// data msg of the local segments, one or two, to addr under key in the
	// peer's memory. The link does not keep sgl.
	PostWriteMsg(id uint64, sgl []Segment, msg Msg, addr uint64, key uint32) error
	// PostMsg mirrors rs_post_msg: the message word msg alone, as an RDMA
	// write with immediate data of no bytes.
	PostMsg(id uint64, msg Msg) error
	// PostRecv mirrors rs_post_recv: one receive, without a buffer, for the
	// next message word.
	PostRecv(id uint64) error
}
