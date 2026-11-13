package rdma

import (
	"context"
	"net/netip"
)

// engine carries the connections of Dial and Listen: platformEngine returns
// the platform's, and tests inject theirs through Dialer and ListenConfig.
type engine interface {
	// dial connects from laddr, nil letting the engine pick, to raddr; ctx
	// cancels the attempt.
	dial(ctx context.Context, laddr, raddr *Addr, c Config) (stream, error)
	// listen announces on laddr; backlog bounds the connection requests
	// waiting to be accepted, 0 meaning the default.
	listen(laddr *Addr, backlog int, c Config) (acceptor, error)
}

// acceptor hands out the connections that reach a listening endpoint.
type acceptor interface {
	// addr returns the address actually bound, the zero AddrPort when
	// unknown.
	addr() netip.AddrPort
	// accept waits for the next connection; close cancels the wait.
	accept() (stream, error)
	// close stops listening: a waiting or later accept returns
	// net.ErrClosed. A second close does nothing.
	close() error
}

// stream is a connected byte stream whose only blocking call is wait. The
// front end makes the calls of one direction one at a time, read and a
// lender's lendRead and returnRead in one, write, lendWrite and commitWrite
// in the other and wait in the direction it names, and lets the two
// directions run concurrently; closeWrite and close may come at any time.
type stream interface {
	// addrs returns the local and the peer's address, the zero AddrPort for
	// one unknown.
	addrs() (local, remote netip.AddrPort)
	// read reads what has arrived: errAgain when nothing has, io.EOF once
	// the peer has shut down writing and everything before has been read.
	read(p []byte) (int, error)
	// write writes what fits and returns how much, errAgain when nothing
	// does.
	write(p []byte) (int, error)
	// wait blocks until the stream is ready in dir, returning nil, or
	// earlier when cancel is closed, nil too, or when the stream is closed,
	// net.ErrClosed. Ready means that the call it waits for would not
	// report errAgain, and the caller asks again either way.
	wait(dir direction, cancel <-chan struct{}) error
	// closeWrite shuts down the sending side: the peer reads io.EOF after
	// the data written so far, and the calls of that side fail from then
	// on, the commit of a view lent before included.
	closeWrite() error
	// close releases the stream: waits return net.ErrClosed, so do the
	// calls that follow, and a second close does nothing.
	close() error
}

// lender is the capability of a stream to lend its own buffers, through
// which ReadFrom and WriteTo then move their bytes. A view stays valid until
// it is returned, even when the stream is closed meanwhile, and the front end
// returns every view it takes, at most one in each direction at a time and
// with no other call in that direction while it is out.
type lender interface {
	// lendRead lends the bytes that have arrived and lie contiguous in the
	// receive buffer: errAgain when none has, io.EOF once the peer has shut
	// down writing and everything before has been read.
	lendRead() ([]byte, error)
	// returnRead returns the read view with its first n bytes consumed,
	// which frees their room for the peer.
	returnRead(n int) error
	// lendWrite lends contiguous room of the send buffer that can be sent at
	// once: errAgain when there is none.
	lendWrite() ([]byte, error)
	// commitWrite returns the write view and sends its first n bytes.
	commitWrite(n int) error
}

// direction is what a wait on a stream waits for.
type direction int

const (
	readable direction = iota // data, or the end of it, to read
	writable                  // room to write
)
