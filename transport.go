package rdma

import (
	"context"
	"net/netip"
)

// transport is the seam between the protocol engine and the fabric: a
// connected, reliable, ordered message channel with posted receive buffers,
// the shape of an RC queue pair driven with SEND and RECV. The engine owns
// the buffer layout: every buffer is MessageSize bytes long, send buffers are
// numbered 0..SendDepth (the last one is the control slot) and receive
// buffers 0..ReceiveDepth, all of which are posted when the transport is
// handed to the engine.
type transport interface {
	// sendSlot returns send buffer i.
	sendSlot(i int) []byte
	// recvSlot returns receive buffer i.
	recvSlot(i int) []byte
	// postSend sends the first n bytes of send buffer i to the peer; a
	// completion with send set and the slot follows once the buffer may be
	// reused.
	postSend(i, n int) error
	// postRecv offers receive buffer i for the next incoming message; a
	// completion with the slot and the message length follows when one lands
	// in it.
	postRecv(i int) error
	// wait blocks until at least one completion is available and stores
	// them in wcs. It returns ctx.Err() when ctx ends first, and the error
	// that broke the transport (the peer disconnected, retries were
	// exhausted) once every completion that preceded it has been returned.
	wait(ctx context.Context, wcs []completion) (int, error)
	// disconnect tells the peer the connection is over, waiting for its
	// answer at most until ctx ends.
	disconnect(ctx context.Context) error
	// close releases every resource of the transport; no other method may
	// be running or called afterwards.
	close() error
	localAddr() netip.AddrPort
	remoteAddr() netip.AddrPort
}

// completion reports that a send buffer is free again (send set) or that a
// message of n bytes landed in a receive buffer.
type completion struct {
	send bool
	slot int
	n    int
}

// fabric sets transports up: on Linux through cm and verbs, in tests in
// memory. The hello and reply frames travel in the connection manager's
// private data and arrive zero padded.
type fabric interface {
	// dial connects to remote from local, or from any address when local is
	// the zero value, with buffers sized by cfg, sending hello. It returns
	// the transport with all receive buffers posted and the peer's reply.
	dial(ctx context.Context, local, remote netip.AddrPort, cfg Config, hello []byte) (transport, []byte, error)
	// listen starts accepting connection requests on local; port 0 picks a
	// free port and backlog 0 the default queue length.
	listen(local netip.AddrPort, backlog int) (acceptor, error)
}

// acceptor hands out connection requests until it is closed.
type acceptor interface {
	addr() netip.AddrPort
	// accept returns the next request. It returns ctx.Err() when ctx ends
	// first and an error wrapping net.ErrClosed once the acceptor is closed.
	accept(ctx context.Context) (request, error)
	close() error
}

// request is a connection request awaiting the handshake decision.
type request interface {
	hello() []byte
	remoteAddr() netip.AddrPort
	// accept sets the connection up with buffers sized by cfg and completes
	// the handshake with reply; the transport's receive buffers are posted.
	// The request is consumed, whether or not the connection came up.
	accept(ctx context.Context, cfg Config, reply []byte) (transport, error)
	// reject refuses the request, handing data to the dialer, and releases
	// it.
	reject(data []byte) error
}
