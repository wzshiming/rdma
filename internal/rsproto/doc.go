// Package rsproto is the stream protocol of rsockets, the socket layer of
// librdmacm, as librdmacm/rsocket.c of rdma-core v66.0 speaks it over
// InfiniBand and RoCE, byte for byte, so that the peer may be any rsockets
// program. It does no I/O: a Stream, the state machine of one connection,
// posts to the transport through a Link and learns what became of its posts
// from the engine that drives it.
//
// Three formats travel on the wire: the message word, the immediate data of
// every message; the grant entry, which a receiver writes into the sender's
// grant table to give it room in its receive buffer; and the connection
// data, which each side puts in the private data of the connection request
// and reply. Direct I/O (riomap), the iWARP mode and datagrams are left out.
//
// # Driving a stream
//
// The engine registers the four blocks of memory of a Config, the receive
// buffer of an even length as it is granted in halves, and builds the stream
// on them with NewStream, which posts the receives of the receive queue. It
// sends the stream's ConnData in the private data of the connection request
// or reply and hands the peer's, read with DecodeConnData, to Connect, which
// grants the peer the second half of the receive buffer. From then on Reserve
// lends room of the send buffer that Commit sends, Peek lends the bytes that
// have arrived and Consume returns them, which earns the peer room and
// credits, and CloseWrite and Disconnect end the write side or both. A view
// stays valid until it is returned, whatever happens meanwhile. Writable,
// Readable and SendsDone tell when a call that would wait, as rsend, rrecv
// and rshutdown wait, can go on, EOF tells the end of the read side and Err
// why the stream ended. The stream has no lock, no goroutine and no clock:
// its caller makes one call at a time, waits on its own and asks again after
// every event. The calls that change the stream may post to the link, which
// must not call back into it.
//
// # Events
//
// The engine hands the stream what the transport reports, in this order:
// each message word that arrives, the immediate data of a receive completion
// in host byte order, to Received, in the order of arrival; each completion
// of a post, with the id it was posted with and its failure if it failed, to
// Sent, in the order of the posts; and the loss of the link to LinkDown once
// the completions before it have been handed over, so that an orderly end
// the peer sent first is taken as such. Arrivals and completions may come in
// any order between each other. A receive that completes in error carries no
// message and is dropped, as rs_poll_cq drops it. The MemEnd of a link
// NewMemLink holds in memory yields the events in that order: Deliver for
// Received, Completion for Sent and Down for LinkDown.
package rsproto
