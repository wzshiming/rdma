package rdma

import "io"

// copyBufferSize is the size of the buffer a copy moves its bytes through.
const copyBufferSize = 32 << 10

// ReadFrom implements io.ReaderFrom, which io.Copy to the connection uses: it
// sends what r reads, each read as soon as it returns, until r reports
// io.EOF, and then returns the number of bytes sent and nil. Errors are
// *net.OpError values of Op "readfrom", r's own included.
func (c *Conn) ReadFrom(r io.Reader) (int64, error) {
	c.wmu.Lock()
	defer c.wmu.Unlock()
	n, err := c.readFrom(r)
	if err != nil {
		return n, c.opError("readfrom", err)
	}
	return n, nil
}

// readFrom copies r to the stream through a buffer. The caller holds wmu.
func (c *Conn) readFrom(r io.Reader) (int64, error) {
	buf := make([]byte, copyBufferSize)
	var total int64
	for {
		n, rerr := r.Read(buf)
		if n > 0 {
			m, err := c.write(buf[:n])
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

// WriteTo implements io.WriterTo, which io.Copy from the connection uses: it
// hands what arrives to w until the peer has shut down writing and
// everything before has been handed over, and then returns the number of
// bytes w took and nil. A w that takes less than it is given without an
// error ends it with io.ErrShortWrite. Errors are *net.OpError values of Op
// "writeto", w's own included.
func (c *Conn) WriteTo(w io.Writer) (int64, error) {
	c.rmu.Lock()
	defer c.rmu.Unlock()
	n, err := c.writeTo(w)
	if err != nil {
		return n, c.opError("writeto", err)
	}
	return n, nil
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
