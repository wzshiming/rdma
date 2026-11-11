package rdma

import (
	"errors"
	"io"
)

// copyBufferSize is the size of the buffer a copy moves its bytes through
// when the stream does not lend its own.
const copyBufferSize = 32 << 10

// ReadFrom implements io.ReaderFrom, which io.Copy to the connection uses: it
// sends what r reads, each read as soon as it returns, until r reports
// io.EOF, and then returns the number of bytes sent and nil. Errors are
// *net.OpError values of Op "readfrom", r's own included.
func (c *Conn) ReadFrom(r io.Reader) (int64, error) {
	c.wmu.Lock()
	defer c.wmu.Unlock()
	var n int64
	var err error
	if l, ok := c.s.(lender); ok {
		n, err = c.readFromLent(l, r)
	} else {
		n, err = c.readFrom(r)
	}
	if err != nil {
		return n, c.opError("readfrom", err)
	}
	return n, nil
}

// readFromLent has r read into the room the stream lends. The caller holds
// wmu.
func (c *Conn) readFromLent(l lender, r io.Reader) (int64, error) {
	var total int64
	for {
		view, err := c.lendWrite(l)
		if err != nil {
			return total, err
		}
		n, rerr := r.Read(view)
		if err := l.commitWrite(n); err != nil {
			return total, err
		}
		total += int64(n)
		if rerr == io.EOF {
			return total, nil
		}
		if rerr != nil {
			return total, rerr
		}
	}
}

// lendWrite borrows room from l, waiting for it within the write deadline.
// The caller holds wmu.
func (c *Conn) lendWrite(l lender) ([]byte, error) {
	for {
		changed, err := c.writeState()
		if err != nil {
			return nil, err
		}
		view, err := l.lendWrite()
		if !errors.Is(err, errAgain) {
			return view, err
		}
		if err := c.s.wait(writable, changed); err != nil {
			return nil, err
		}
	}
}

// readFrom copies r to the stream through a buffer. The caller holds wmu.
func (c *Conn) readFrom(r io.Reader) (int64, error) {
	buf := make([]byte, copyBufferSize)
	var total int64
	for {
		if _, err := c.writeState(); err != nil {
			return total, err
		}
		n, rerr := r.Read(buf)
		if n > 0 {
			m, err := c.send(buf[:n])
			total += int64(m)
			if err != nil {
				return total, err
			}
		}
		if rerr == io.EOF {
			return total, nil
		}
		if rerr != nil {
			return total, rerr
		}
	}
}

// send writes p, which the source has already given up: what fits goes at
// once, the deadline notwithstanding, as it would into lent room, and the
// rest as write sends it. The caller holds wmu.
func (c *Conn) send(p []byte) (int, error) {
	if err := c.writeClosedError(); err != nil {
		return 0, err
	}
	n, err := c.s.write(p)
	if err != nil && !errors.Is(err, errAgain) {
		return n, err
	}
	if n == len(p) {
		return n, nil
	}
	m, err := c.write(p[n:])
	return n + m, err
}

// WriteTo implements io.WriterTo, which io.Copy from the connection uses: it
// hands what arrives to w until the peer has shut down writing and
// everything before has been handed over, and then returns the number of
// bytes w took and nil. A w that takes less than it is given without an
// error ends it with io.ErrShortWrite. Errors are *net.OpError values of Op
// "writeto", w's own included.
func (c *Conn) WriteTo(w io.Writer) (int64, error) {
	c.rmu.Lock()
	defer c.rmu.Unlock()
	var n int64
	var err error
	if l, ok := c.s.(lender); ok {
		n, err = c.writeToLent(l, w)
	} else {
		n, err = c.writeTo(w)
	}
	if err != nil {
		return n, c.opError("writeto", err)
	}
	return n, nil
}

// writeToLent hands w the bytes the stream lends and consumes what w takes.
// The caller holds rmu.
func (c *Conn) writeToLent(l lender, w io.Writer) (int64, error) {
	var total int64
	for {
		view, err := c.lendRead(l)
		if err == io.EOF {
			return total, nil
		}
		if err != nil {
			return total, err
		}
		n, werr := w.Write(view)
		total += int64(n)
		if err := l.returnRead(n); err != nil {
			return total, err
		}
		if werr != nil {
			return total, werr
		}
		if n < len(view) {
			return total, io.ErrShortWrite
		}
	}
}

// lendRead borrows the bytes that have arrived from l, waiting for them
// within the read deadline. The caller holds rmu.
func (c *Conn) lendRead(l lender) ([]byte, error) {
	for {
		changed, err := c.readState()
		if err != nil {
			return nil, err
		}
		view, err := l.lendRead()
		if !errors.Is(err, errAgain) {
			return view, err
		}
		if err := c.s.wait(readable, changed); err != nil {
			return nil, err
		}
	}
}

// writeTo copies the stream to w through a buffer. The caller holds rmu.
func (c *Conn) writeTo(w io.Writer) (int64, error) {
	buf := make([]byte, copyBufferSize)
	var total int64
	for {
		n, err := c.read(buf)
		if n > 0 {
			m, werr := w.Write(buf[:n])
			total += int64(m)
			if werr != nil {
				return total, werr
			}
			if m < n {
				return total, io.ErrShortWrite
			}
		}
		if err == io.EOF {
			return total, nil
		}
		if err != nil {
			return total, err
		}
	}
}
