package rdma

import (
	"context"
	"net/netip"
)

// engine carries the connections of Dial and Listen: platformEngine picks the
// one for a Config, and tests inject theirs through Dialer and ListenConfig.
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
// front end makes the calls of one direction one at a time, read in one,
// write in the other and wait in the direction it names, and lets the two
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
	// the data written so far.
	closeWrite() error
	// close releases the stream: waits return net.ErrClosed, so do the
	// calls that follow, and a second close does nothing.
	close() error
}

// direction is what a wait on a stream waits for.
type direction int

const (
	readable direction = iota // data, or the end of it, to read
	writable                  // room to write
)
