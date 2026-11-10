package rdma

import (
	"errors"
	"io"
	"net"
	"os"
	"sync"
	"time"

	"github.com/wzshiming/rdma/rsocket"
)

// Conn is an RDMA stream connection, an rsocket driven in non-blocking mode.
// It implements net.Conn: Read and Write may run concurrently with each
// other, calls in the same direction are serialised, and Close wakes them
// all.
type Conn struct {
	nw            network
	s             socket
	local, remote net.Addr

	rmu sync.Mutex // serialises Read calls
	wmu sync.Mutex // serialises Write and CloseWrite calls

	mu          sync.Mutex
	closed      bool
	writeClosed bool
	closing     chan struct{} // closed by Close, waking every wait
	rd, wd      deadline
}

// newConn wraps a connected socket attached to the network's poller.
func newConn(nw network, s socket) *Conn {
	c := &Conn{nw: nw, s: s, closing: make(chan struct{})}
	nw.run(s, func() {
		if a, err := s.LocalAddr(); err == nil {
			if addr := addrFromAddrPort(a); addr != nil {
				c.local = addr
			}
		}
		if a, err := s.RemoteAddr(); err == nil {
			if addr := addrFromAddrPort(a); addr != nil {
				c.remote = addr
			}
		}
	})
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

// await blocks until events hold on the socket, Close is called or the
// deadline passes, reporting the last two.
func (c *Conn) await(events rsocket.Events, d *deadline) error {
	for {
		expired, changed := d.state()
		if expired {
			return os.ErrDeadlineExceeded
		}
		ready, cancel := c.nw.wait(c.s, events)
		select {
		case <-ready:
			return nil
		case <-c.closing:
			cancel()
			return net.ErrClosed
		case <-changed:
			cancel()
		}
	}
}

// Read reads data from the connection. It returns io.EOF once the peer has
// closed its sending side and every byte before that has been read.
func (c *Conn) Read(p []byte) (int, error) {
	c.rmu.Lock()
	defer c.rmu.Unlock()
	for {
		if c.isClosed() {
			return 0, c.opError("read", net.ErrClosed)
		}
		if expired, _ := c.rd.state(); expired {
			return 0, c.opError("read", os.ErrDeadlineExceeded)
		}
		var n int
		var err error
		if !c.nw.run(c.s, func() { n, err = c.s.Read(p) }) {
			return 0, c.opError("read", net.ErrClosed)
		}
		switch {
		case err == nil || err == io.EOF:
			return n, err
		case !errors.Is(err, errAgain):
			return n, c.opError("read", err)
		}
		if err := c.await(rsocket.PollIn, &c.rd); err != nil {
			return 0, c.opError("read", err)
		}
	}
}

// Write writes data to the connection. It returns once every byte has been
// handed to rsockets, which copy it into the registered send buffer.
func (c *Conn) Write(p []byte) (int, error) {
	c.wmu.Lock()
	defer c.wmu.Unlock()
	total := 0
	for {
		c.mu.Lock()
		closed, writeClosed := c.closed, c.writeClosed
		c.mu.Unlock()
		switch {
		case closed:
			return total, c.opError("write", net.ErrClosed)
		case writeClosed:
			return total, c.opError("write", errPipe)
		}
		if expired, _ := c.wd.state(); expired {
			return total, c.opError("write", os.ErrDeadlineExceeded)
		}
		if len(p) == 0 {
			return total, nil
		}
		var n int
		var err error
		if !c.nw.run(c.s, func() { n, err = c.s.Write(p) }) {
			return total, c.opError("write", net.ErrClosed)
		}
		total += n
		p = p[n:]
		if err != nil && !errors.Is(err, errAgain) {
			return total, c.opError("write", err)
		}
		if err == nil && n > 0 {
			continue
		}
		if err := c.await(rsocket.PollOut, &c.wd); err != nil {
			return total, c.opError("write", err)
		}
	}
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
	// Shutdown may block, so it runs here rather than on the poller, which
	// leaves the socket alone meanwhile.
	c.nw.exclude(c.s)
	err := c.s.Shutdown(rsocket.ShutWrite)
	c.nw.include(c.s)
	if err != nil {
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
	close(c.closing)
	c.rd.stop()
	c.wd.stop()
	c.mu.Unlock()
	// The poller must have let go of the descriptor before rclose frees it.
	c.nw.detach(c.s)
	if err := c.s.Close(); err != nil {
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
