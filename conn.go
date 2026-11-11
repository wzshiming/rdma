package rdma

import (
	"errors"
	"io"
	"net"
	"os"
	"sync"
	"time"
)

// Conn is an RDMA stream connection, an rsocket driven in non-blocking mode.
// It implements net.Conn: Read and Write may run concurrently with each
// other, calls in the same direction are serialised, and Close wakes them
// all.
type Conn struct {
	s             stream
	local, remote net.Addr

	rmu sync.Mutex // serialises Read calls
	wmu sync.Mutex // serialises Write and CloseWrite calls

	mu          sync.Mutex
	closed      bool
	writeClosed bool
	rd, wd      deadline
}

// newConn wraps a connected stream.
func newConn(s stream) *Conn {
	c := &Conn{s: s}
	local, remote := s.addrs()
	if addr := addrFromAddrPort(local); addr != nil {
		c.local = addr
	}
	if addr := addrFromAddrPort(remote); addr != nil {
		c.remote = addr
	}
	return c
}

// opError wraps err the way the net package reports errors; a socket closed
// underneath reads as net.ErrClosed.
func (c *Conn) opError(op string, err error) error {
	if errors.Is(err, os.ErrClosed) {
		err = net.ErrClosed
	}
	return &net.OpError{Op: op, Net: "rdma", Source: c.local, Addr: c.remote, Err: err}
}

func (c *Conn) isClosed() bool {
	c.mu.Lock()
	defer c.mu.Unlock()
	return c.closed
}

// Read reads data from the connection. It returns io.EOF once the peer has
// closed its sending side and every byte before that has been read.
func (c *Conn) Read(p []byte) (int, error) {
	c.rmu.Lock()
	defer c.rmu.Unlock()
	n, err := c.read(p)
	if err != nil && err != io.EOF {
		return n, c.opError("read", err)
	}
	return n, err
}

// read reads from the stream, waiting for data within the read deadline. The
// caller holds rmu.
func (c *Conn) read(p []byte) (int, error) {
	for {
		changed, err := c.readState()
		if err != nil {
			return 0, err
		}
		n, err := c.s.read(p)
		if !errors.Is(err, errAgain) {
			return n, err
		}
		if err := c.s.wait(readable, changed); err != nil {
			return 0, err
		}
	}
}

// readState reports why reading cannot go on, if it cannot, and returns the
// channel that a change of the read deadline closes.
func (c *Conn) readState() (changed <-chan struct{}, err error) {
	if c.isClosed() {
		return nil, net.ErrClosed
	}
	expired, changed := c.rd.state()
	if expired {
		return nil, os.ErrDeadlineExceeded
	}
	return changed, nil
}

// Write writes data to the connection. It returns once every byte has been
// handed to rsockets, which copy it into the registered send buffer.
func (c *Conn) Write(p []byte) (int, error) {
	c.wmu.Lock()
	defer c.wmu.Unlock()
	n, err := c.write(p)
	if err != nil {
		return n, c.opError("write", err)
	}
	return n, nil
}

// write writes all of p to the stream, waiting for room within the write
// deadline. The caller holds wmu.
func (c *Conn) write(p []byte) (int, error) {
	total := 0
	for {
		changed, err := c.writeState()
		if err != nil {
			return total, err
		}
		if len(p) == 0 {
			return total, nil
		}
		n, err := c.s.write(p)
		total += n
		p = p[n:]
		if err != nil && !errors.Is(err, errAgain) {
			return total, err
		}
		if err == nil && n > 0 {
			continue
		}
		if err := c.s.wait(writable, changed); err != nil {
			return total, err
		}
	}
}

// writeState reports why writing cannot go on, if it cannot, and returns the
// channel that a change of the write deadline closes.
func (c *Conn) writeState() (changed <-chan struct{}, err error) {
	if err := c.writeClosedError(); err != nil {
		return nil, err
	}
	expired, changed := c.wd.state()
	if expired {
		return nil, os.ErrDeadlineExceeded
	}
	return changed, nil
}

// writeClosedError returns the error of a write after Close or CloseWrite.
func (c *Conn) writeClosedError() error {
	c.mu.Lock()
	defer c.mu.Unlock()
	switch {
	case c.closed:
		return net.ErrClosed
	case c.writeClosed:
		return errPipe
	}
	return nil
}

// CloseWrite closes the sending side: the peer reads io.EOF after the data
// written so far, later Writes fail with EPIPE, and Read keeps working. It
// waits for the posted sends to complete, which may take the transport's
// retry timeout when the peer has vanished.
func (c *Conn) CloseWrite() error {
	c.wmu.Lock()
	defer c.wmu.Unlock()
	c.mu.Lock()
	switch {
	case c.closed:
		c.mu.Unlock()
		return c.opError("close", net.ErrClosed)
	case c.writeClosed:
		c.mu.Unlock()
		return nil
	}
	c.writeClosed = true
	c.mu.Unlock()
	if err := c.s.closeWrite(); err != nil {
		return c.opError("close", err)
	}
	return nil
}

// Close closes the connection: blocked Reads and Writes return net.ErrClosed,
// the peer reads io.EOF after the data written so far, and the socket is
// released. Like CloseWrite it waits for the posted sends to complete.
func (c *Conn) Close() error {
	c.mu.Lock()
	if c.closed {
		c.mu.Unlock()
		return c.opError("close", net.ErrClosed)
	}
	c.closed = true
	c.rd.stop()
	c.wd.stop()
	c.mu.Unlock()
	if err := c.s.close(); err != nil {
		return c.opError("close", err)
	}
	return nil
}

// LocalAddr returns the local address, an *Addr.
func (c *Conn) LocalAddr() net.Addr { return c.local }

// RemoteAddr returns the peer's address, an *Addr.
func (c *Conn) RemoteAddr() net.Addr { return c.remote }

// SetDeadline sets the read and write deadlines, as net.Conn describes.
func (c *Conn) SetDeadline(t time.Time) error {
	if c.isClosed() {
		return c.opError("set", net.ErrClosed)
	}
	c.rd.set(t)
	c.wd.set(t)
	return nil
}

// SetReadDeadline sets the deadline for Read calls.
func (c *Conn) SetReadDeadline(t time.Time) error {
	if c.isClosed() {
		return c.opError("set", net.ErrClosed)
	}
	c.rd.set(t)
	return nil
}

// SetWriteDeadline sets the deadline for Write calls.
func (c *Conn) SetWriteDeadline(t time.Time) error {
	if c.isClosed() {
		return c.opError("set", net.ErrClosed)
	}
	c.wd.set(t)
	return nil
}

// deadline is one direction's deadline. A waiting call selects on the
// channel state returns, which closes when the deadline passes or is
// replaced, and then asks again.
type deadline struct {
	mu      sync.Mutex
	timer   *time.Timer
	ch      chan struct{} // closed on expiry or change, then replaced
	expired bool
}

// state reports whether the deadline has passed and returns the channel
// closed by its next expiry or change.
func (d *deadline) state() (expired bool, changed <-chan struct{}) {
	d.mu.Lock()
	defer d.mu.Unlock()
	if d.ch == nil {
		d.ch = make(chan struct{})
	}
	return d.expired, d.ch
}

// set arms the deadline t, a zero t clearing it, and wakes the waiters so
// that they take the change into account.
func (d *deadline) set(t time.Time) {
	d.mu.Lock()
	defer d.mu.Unlock()
	d.disarm()
	d.expired = false
	d.wake()
	if t.IsZero() {
		return
	}
	wait := time.Until(t)
	if wait <= 0 {
		d.expired = true
		return
	}
	ch := d.ch
	d.timer = time.AfterFunc(wait, func() {
		d.mu.Lock()
		defer d.mu.Unlock()
		// A deadline set since then has its own timer.
		if d.ch == ch {
			d.expired = true
			d.wake()
		}
	})
}

// stop disarms the deadline when the connection closes.
func (d *deadline) stop() {
	d.mu.Lock()
	defer d.mu.Unlock()
	d.disarm()
}

// disarm stops the timer. The caller holds mu.
func (d *deadline) disarm() {
	if d.timer != nil {
		d.timer.Stop()
		d.timer = nil
	}
}

// wake closes the current channel and replaces it. The caller holds mu.
func (d *deadline) wake() {
	if d.ch != nil {
		close(d.ch)
	}
	d.ch = make(chan struct{})
}
