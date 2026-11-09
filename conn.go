package rdma

import (
	"context"
	"encoding/binary"
	"fmt"
	"io"
	"net"
	"os"
	"sync"
	"time"
)

// The wire protocol.
//
// A connection is an RC queue pair driven with SEND and RECV only, so it runs
// on InfiniBand, RoCE and iWARP alike. Both ends exchange messages of at most
// MessageSize bytes (the smaller of the two configured values, agreed in the
// handshake), each opening with a 16-byte header:
//
//	0  flags: data, EOF or control (exactly one)
//	1  reserved (3 bytes)
//	4  credits: data receive buffers the sender has re-posted since its last
//	   message (uint32, big endian)
//	8  control ack: control messages the sender has consumed so far (uint32)
//	12 payload length (uint32)
//
// Flow control is credit based (I1): the receiver posts ReceiveDepth data
// buffers plus one control slot, and the sender starts with ReceiveDepth
// data credits and one control credit. Every data or EOF message costs a
// data credit (I6: EOF is an explicit message and no data follows it), every
// control message the control credit. A buffer is re-posted before the
// credit for it is announced (I5), so the number of posted buffers never
// drops below the credits the peer holds plus its messages in flight, and no
// message ever arrives without a buffer waiting for it. The real transport
// still sets rnr_retry 7, but the protocol never leans on it, and the fake
// transport fails a test when the invariant breaks.
//
// Credits travel in the header of whatever the receiver sends next (I3):
// piggybacked on data whenever there is data, otherwise in a control
// message. The peer frees the control slot by re-posting the buffer at once
// and sending the count of control messages it has consumed in the control
// ack field of its next message; the sender may have one control message in
// flight and regains the credit when the ack equals its own count.
//
// Deadlock freedom, including both ends at zero data credits and
// ReceiveDepth 1: a control credit is only missing while a control message of
// ours is unacknowledged, and the peer acknowledges with its next message of
// any kind. That control message carried at least one data credit, which the
// peer still holds unless it has sent since, in which case it acknowledged.
// So whenever we hold grants we cannot announce, the peer can send; and
// whenever the peer is out of data credits, every buffer it filled sits in
// our queue or has been re-posted, and reading grants it credits either in a
// control message (we hold the credit, by the argument above) or piggybacked
// on our own data. Grants never depend on the granting side's application
// writing or on the peer reading, only on the granting side reading, which is
// the same dependency a TCP window has.
//
// Grants are batched to half the receive depth, or sent at once when the
// peer is known to hold no credit at all.

// headerSize is the size of the message header.
const headerSize = 16

// Message flags.
const (
	flagData    uint8 = 1 << 0
	flagEOF     uint8 = 1 << 1
	flagControl uint8 = 1 << 2
)

// header is the decoded message header.
type header struct {
	flags   uint8
	credits uint32
	ctrlAck uint32
	length  uint32
}

func putHeader(b []byte, h header) {
	b[0] = h.flags
	b[1], b[2], b[3] = 0, 0, 0
	binary.BigEndian.PutUint32(b[4:], h.credits)
	binary.BigEndian.PutUint32(b[8:], h.ctrlAck)
	binary.BigEndian.PutUint32(b[12:], h.length)
}

func getHeader(b []byte) header {
	return header{
		flags:   b[0],
		credits: binary.BigEndian.Uint32(b[4:]),
		ctrlAck: binary.BigEndian.Uint32(b[8:]),
		length:  binary.BigEndian.Uint32(b[12:]),
	}
}

// Timing of Close: how long it waits for the EOF message to go out and for
// the peer to answer the disconnect.
const (
	closeLinger       = 500 * time.Millisecond
	disconnectTimeout = time.Second
)

// Conn is an RDMA stream connection. It implements net.Conn: Read and Write
// may run concurrently with each other, and calls in the same direction are
// serialised.
type Conn struct {
	t             transport
	local, remote net.Addr
	msgSize       int // negotiated message size
	payload       int // bytes of data per message
	recvDepth     int // negotiated receive depth, the peer's initial credits
	sendDepth     int // data send slots; slot sendDepth is the control slot
	grantBatch    int // pending grants that trigger a control message

	rmu sync.Mutex // serialises Read calls
	wmu sync.Mutex // serialises Write calls

	mu    sync.Mutex
	rcond sync.Cond // data queued, EOF, failure, close or read deadline
	wcond sync.Cond // send slot or credit freed, failure, close or write deadline

	// Receive side.
	recvq         []recvMsg // messages waiting to be read, oldest first
	recvEOF       bool      // the peer sent EOF
	pendingGrants int       // buffers re-posted but not yet announced
	announced     int       // data credits announced to the peer in total
	received      int       // data and EOF messages received in total
	ctrlRecv      uint32    // control messages consumed, sent as the ack

	// Send side.
	freeSlots   []int // data send slots not in flight
	dataCredits int
	ctrlCredit  bool // the peer's control slot is free
	ctrlBusy    bool // the control send slot is in flight
	ctrlSent    uint32
	eofPending  bool // EOF requested, waiting for a slot and a credit
	sentEOF     bool
	eofSlot     int
	eofDone     chan struct{} // closed once the EOF message completed, or never will
	eofDoneOnce sync.Once

	// Life cycle.
	closed     bool
	err        error // the transport failed
	pumpCtx    context.Context
	pumpCancel context.CancelFunc
	pumpDone   chan struct{}
	rd, wd     deadline
}

// recvMsg is a received data message: n payload bytes in slot, off of them
// already read.
type recvMsg struct {
	slot, off, n int
}

// newConn starts the engine on t, whose buffers follow cfg: the negotiated
// MessageSize and ReceiveDepth, and this end's SendDepth.
func newConn(t transport, cfg Config) *Conn {
	c := &Conn{
		t:           t,
		msgSize:     cfg.MessageSize,
		payload:     cfg.MessageSize - headerSize,
		recvDepth:   cfg.ReceiveDepth,
		sendDepth:   cfg.SendDepth,
		grantBatch:  max(1, cfg.ReceiveDepth/2),
		dataCredits: cfg.ReceiveDepth,
		announced:   cfg.ReceiveDepth,
		ctrlCredit:  true,
		eofDone:     make(chan struct{}),
		pumpDone:    make(chan struct{}),
	}
	if a := addrFromAddrPort(t.localAddr()); a != nil {
		c.local = a
	}
	if a := addrFromAddrPort(t.remoteAddr()); a != nil {
		c.remote = a
	}
	c.rcond.L = &c.mu
	c.wcond.L = &c.mu
	c.freeSlots = make([]int, 0, cfg.SendDepth)
	for i := range cfg.SendDepth {
		c.freeSlots = append(c.freeSlots, i)
	}
	c.pumpCtx, c.pumpCancel = context.WithCancel(context.Background())
	go c.pump()
	return c
}

// opError wraps err the way the net package reports errors.
func (c *Conn) opError(op string, err error) error {
	return &net.OpError{Op: op, Net: "rdma", Source: c.local, Addr: c.remote, Err: err}
}

// pump moves completions from the transport into the engine until the
// transport fails or Close cancels it.
func (c *Conn) pump() {
	defer close(c.pumpDone)
	wcs := make([]completion, min(c.sendDepth+c.recvDepth+2, 128))
	for {
		n, err := c.t.wait(c.pumpCtx, wcs)
		c.mu.Lock()
		for _, wc := range wcs[:n] {
			c.handle(wc)
		}
		if err != nil {
			if c.pumpCtx.Err() == nil {
				c.fail(err)
			}
			c.mu.Unlock()
			return
		}
		c.mu.Unlock()
	}
}

// handle applies one completion. The caller holds mu.
func (c *Conn) handle(wc completion) {
	if wc.send {
		c.handleSent(wc.slot)
		return
	}
	if c.err != nil {
		return
	}
	b := c.t.recvSlot(wc.slot)
	if wc.n < headerSize {
		c.fail(fmt.Errorf("%w: %d-byte message is shorter than the header", ErrProtocol, wc.n))
		return
	}
	h := getHeader(b)
	if int(h.length) != wc.n-headerSize {
		c.fail(fmt.Errorf("%w: header announces %d payload bytes in a %d-byte message", ErrProtocol, h.length, wc.n))
		return
	}
	// Credits and the control ack ride on every message.
	c.dataCredits = min(c.dataCredits+int(h.credits), c.recvDepth)
	if h.ctrlAck == c.ctrlSent {
		c.ctrlCredit = true
	}
	if h.credits > 0 {
		c.trySendEOF()
		c.wcond.Broadcast()
	}
	switch h.flags {
	case flagControl:
		// The control slot goes back to the peer through the ack, not as a
		// data credit.
		c.ctrlRecv++
		if !c.closed {
			if err := c.t.postRecv(wc.slot); err != nil {
				c.fail(err)
				return
			}
		}
	case flagEOF:
		c.received++
		c.recvEOF = true
		c.rcond.Broadcast()
	case flagData:
		c.received++
		if !c.closed {
			c.recvq = append(c.recvq, recvMsg{slot: wc.slot, n: int(h.length)})
			c.rcond.Broadcast()
		}
	default:
		c.fail(fmt.Errorf("%w: message flags %#x", ErrProtocol, h.flags))
		return
	}
	c.maybeGrant()
}

// handleSent frees the send slot a completion names. The caller holds mu.
func (c *Conn) handleSent(slot int) {
	if slot == c.sendDepth {
		c.ctrlBusy = false
		c.maybeGrant()
		return
	}
	c.freeSlots = append(c.freeSlots, slot)
	if c.sentEOF && slot == c.eofSlot {
		c.eofDoneOnce.Do(func() { close(c.eofDone) })
	}
	c.trySendEOF()
	c.wcond.Broadcast()
}

// repost offers a consumed data receive buffer again; its credit is
// announced later. The caller holds mu.
func (c *Conn) repost(slot int) {
	if err := c.t.postRecv(slot); err != nil {
		c.fail(err)
		return
	}
	c.pendingGrants++
}

// maybeGrant sends pending grants in a control message when it is worth it
// and possible: the control credit is held, the control slot is free, and
// either enough grants have gathered or the peer holds no credit at all. The
// caller holds mu.
func (c *Conn) maybeGrant() {
	if c.pendingGrants == 0 || !c.ctrlCredit || c.ctrlBusy || c.closed || c.err != nil || c.recvEOF {
		return
	}
	if c.pendingGrants < c.grantBatch && c.announced-c.received > 0 {
		return
	}
	putHeader(c.t.sendSlot(c.sendDepth), header{flags: flagControl, credits: uint32(c.pendingGrants), ctrlAck: c.ctrlRecv})
	c.announced += c.pendingGrants
	c.pendingGrants = 0
	c.ctrlCredit = false
	c.ctrlBusy = true
	c.ctrlSent++
	if err := c.t.postSend(c.sendDepth, headerSize); err != nil {
		c.fail(err)
	}
}

// sendData posts the data or EOF message prepared in slot, piggybacking the
// pending grants. The caller holds mu and has taken slot from freeSlots.
func (c *Conn) sendData(slot int, flags uint8, n int) {
	putHeader(c.t.sendSlot(slot), header{flags: flags, credits: uint32(c.pendingGrants), ctrlAck: c.ctrlRecv, length: uint32(n)})
	c.announced += c.pendingGrants
	c.pendingGrants = 0
	c.dataCredits--
	if err := c.t.postSend(slot, headerSize+n); err != nil {
		c.fail(err)
	}
}

// trySendEOF posts the EOF message once one is requested and a slot and a
// credit are available. The caller holds mu.
func (c *Conn) trySendEOF() {
	if !c.eofPending || c.sentEOF || c.err != nil || len(c.freeSlots) == 0 || c.dataCredits == 0 {
		return
	}
	slot := c.freeSlots[len(c.freeSlots)-1]
	c.freeSlots = c.freeSlots[:len(c.freeSlots)-1]
	c.eofPending = false
	c.sentEOF = true
	c.eofSlot = slot
	c.sendData(slot, flagEOF, 0)
}

// fail records the first transport or protocol failure and wakes everyone.
// The caller holds mu.
func (c *Conn) fail(err error) {
	if c.err == nil {
		c.err = err
	}
	c.eofDoneOnce.Do(func() { close(c.eofDone) })
	c.rcond.Broadcast()
	c.wcond.Broadcast()
}

// Read reads data from the connection. It returns io.EOF once the peer has
// closed its sending side and every byte before that has been read.
func (c *Conn) Read(p []byte) (int, error) {
	c.rmu.Lock()
	defer c.rmu.Unlock()
	c.mu.Lock()
	defer c.mu.Unlock()
	for {
		switch {
		case c.closed:
			return 0, c.opError("read", net.ErrClosed)
		case c.rd.expired:
			return 0, c.opError("read", os.ErrDeadlineExceeded)
		case len(c.recvq) > 0:
			if len(p) == 0 {
				return 0, nil
			}
			m := &c.recvq[0]
			n := copy(p, c.t.recvSlot(m.slot)[headerSize+m.off:headerSize+m.n])
			m.off += n
			if m.off == m.n {
				slot := m.slot
				c.recvq = c.recvq[1:]
				if len(c.recvq) == 0 {
					c.recvq = c.recvq[:0]
				}
				c.repost(slot)
				c.maybeGrant()
			}
			return n, nil
		case c.recvEOF:
			return 0, io.EOF
		case c.err != nil:
			return 0, c.opError("read", c.err)
		case len(p) == 0:
			return 0, nil
		}
		c.rcond.Wait()
	}
}

// Write writes data to the connection. It returns once every byte has been
// copied into a registered send buffer and posted.
func (c *Conn) Write(p []byte) (int, error) {
	c.wmu.Lock()
	defer c.wmu.Unlock()
	c.mu.Lock()
	defer c.mu.Unlock()
	total := 0
	for {
		switch {
		case c.closed:
			return total, c.opError("write", net.ErrClosed)
		case c.err != nil:
			return total, c.opError("write", c.err)
		case c.sentEOF || c.eofPending:
			return total, c.opError("write", errPipe)
		case c.wd.expired:
			return total, c.opError("write", os.ErrDeadlineExceeded)
		case len(p) == 0:
			return total, nil
		case len(c.freeSlots) > 0 && c.dataCredits > 0:
			slot := c.freeSlots[len(c.freeSlots)-1]
			c.freeSlots = c.freeSlots[:len(c.freeSlots)-1]
			n := copy(c.t.sendSlot(slot)[headerSize:c.msgSize], p)
			c.sendData(slot, flagData, n)
			p = p[n:]
			total += n
			continue
		}
		c.wcond.Wait()
	}
}

// CloseWrite closes the sending side: the peer reads io.EOF after the data
// written so far, later Writes fail, and Read keeps working.
func (c *Conn) CloseWrite() error {
	c.mu.Lock()
	defer c.mu.Unlock()
	switch {
	case c.closed:
		return c.opError("close", net.ErrClosed)
	case c.sentEOF || c.eofPending:
		return nil
	case c.err != nil:
		return c.opError("close", c.err)
	}
	c.eofPending = true
	c.trySendEOF()
	return nil
}

// Close closes the connection: blocked Reads and Writes return net.ErrClosed,
// EOF is sent if the peer can take it within a short time, the connection is
// disconnected and every resource is released.
func (c *Conn) Close() error {
	c.mu.Lock()
	if c.closed {
		c.mu.Unlock()
		return c.opError("close", net.ErrClosed)
	}
	c.closed = true
	c.rd.stop()
	c.wd.stop()
	c.rcond.Broadcast()
	c.wcond.Broadcast()
	if !c.sentEOF && !c.eofPending && c.err == nil {
		c.eofPending = true
	}
	c.trySendEOF()
	if c.err != nil {
		c.eofDoneOnce.Do(func() { close(c.eofDone) })
	}
	c.mu.Unlock()

	linger := time.NewTimer(closeLinger)
	select {
	case <-c.eofDone:
	case <-linger.C:
	}
	linger.Stop()
	ctx, cancel := context.WithTimeout(context.Background(), disconnectTimeout)
	c.t.disconnect(ctx)
	cancel()
	c.pumpCancel()
	<-c.pumpDone
	if err := c.t.close(); err != nil {
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
	c.mu.Lock()
	defer c.mu.Unlock()
	if c.closed {
		return c.opError("set", net.ErrClosed)
	}
	c.rd.set(&c.mu, &c.rcond, t)
	c.wd.set(&c.mu, &c.wcond, t)
	return nil
}

// SetReadDeadline sets the deadline for Read calls.
func (c *Conn) SetReadDeadline(t time.Time) error {
	c.mu.Lock()
	defer c.mu.Unlock()
	if c.closed {
		return c.opError("set", net.ErrClosed)
	}
	c.rd.set(&c.mu, &c.rcond, t)
	return nil
}

// SetWriteDeadline sets the deadline for Write calls.
func (c *Conn) SetWriteDeadline(t time.Time) error {
	c.mu.Lock()
	defer c.mu.Unlock()
	if c.closed {
		return c.opError("set", net.ErrClosed)
	}
	c.wd.set(&c.mu, &c.wcond, t)
	return nil
}

// deadline is one direction's deadline: expired flips when it passes and a
// waiting call is woken through its condition variable.
type deadline struct {
	timer   *time.Timer
	expired bool
	gen     uint64 // invalidates the callback of a replaced timer
}

// set arms the deadline t; the caller holds mu.
func (d *deadline) set(mu *sync.Mutex, cond *sync.Cond, t time.Time) {
	d.stop()
	d.expired = false
	if t.IsZero() {
		return
	}
	wait := time.Until(t)
	if wait <= 0 {
		d.expired = true
		cond.Broadcast()
		return
	}
	gen := d.gen
	d.timer = time.AfterFunc(wait, func() {
		mu.Lock()
		defer mu.Unlock()
		if d.gen == gen {
			d.expired = true
			cond.Broadcast()
		}
	})
}

// stop disarms the deadline's timer; the caller holds mu.
func (d *deadline) stop() {
	d.gen++
	if d.timer != nil {
		d.timer.Stop()
		d.timer = nil
	}
}
