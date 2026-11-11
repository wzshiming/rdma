package rdma

import (
	"io"
	"net"
	"net/netip"
	"sync"
	"time"

	"github.com/wzshiming/rdma/internal/rsproto"
)

// closeTimeout bounds how long close waits for the data posted and the end
// of the connection to reach the peer.
const closeTimeout = time.Second

// transport carries a native stream: the queue pair of a connection, or a
// link held in memory in tests. The stream posts through its rsproto.Link,
// from inside its own calls; the transport feeds the stream what becomes of
// the posts and what arrives through the linkEvents start hands it.
type transport interface {
	rsproto.Link
	// start has the transport feed ev the events of the link from then on,
	// from a goroutine of its own, in the order package rsproto asks for:
	// sent for the completion of each send, received for each message word
	// that arrives, and lost once, for the loss of the link, after the
	// completions before it. Each batch of events comes with ev's lock held.
	start(ev linkEvents)
	// down takes the link down, which the peer learns as its loss, and
	// returns once the transport feeds the stream nothing more and touches
	// its memory no more. It may come without start.
	down()
	// release frees the memory the stream was built on; it comes after down.
	release()
}

// linkEvents is the native stream as the transport under it sees it. The
// transport takes the lock, feeds the events and lets the lock go, which
// wakes whoever the events let go on; it calls nothing else in between.
type linkEvents interface {
	lock()
	unlock()
	sent(id uint64, err error)
	received(msg rsproto.Msg)
	lost(err error)
}

// nativeStream is a stream of the native engine: one rsproto.Stream that
// its transport drives in the background and the front end through the
// calls of the stream interface.
type nativeStream struct {
	t             transport
	local, remote netip.AddrPort
	closing       chan struct{}    // closed by close, waking every wait
	wake          [2]chan struct{} // by direction, one slot for the token that lets a wait go on

	mu        sync.Mutex
	st        *rsproto.Stream
	link      sendCounter
	waiting   [2]bool // by direction, a wait sleeps on wake
	writeShut bool    // closeWrite was called
	closed    bool
	settled   chan struct{} // closed once the sends of a closing stream are done
}

// sendCounter is the Link a native stream posts through: its transport's,
// counting the sends that have yet to complete.
type sendCounter struct {
	rsproto.Link
	pending int
}

func (c *sendCounter) PostWriteMsg(id uint64, sgl []rsproto.Segment, msg rsproto.Msg, addr uint64, key uint32) error {
	err := c.Link.PostWriteMsg(id, sgl, msg, addr, key)
	if err == nil {
		c.pending++
	}
	return err
}

func (c *sendCounter) PostMsg(id uint64, msg rsproto.Msg) error {
	err := c.Link.PostMsg(id, msg)
	if err == nil {
		c.pending++
	}
	return err
}

// newNativeStream builds a native stream on t and the memory of mem, which
// t has registered, between the addresses: the receive queue is posted and
// the stream awaits connect. It fails when t refuses a receive.
func newNativeStream(t transport, mem rsproto.Config, local, remote netip.AddrPort) (*nativeStream, error) {
	s := &nativeStream{
		t:       t,
		local:   local,
		remote:  remote,
		closing: make(chan struct{}),
		wake:    [2]chan struct{}{make(chan struct{}, 1), make(chan struct{}, 1)},
	}
	s.link.Link = t
	s.st = rsproto.NewStream(&s.link, mem)
	if err := s.st.Err(); err != nil {
		return nil, err
	}
	return s, nil
}

// connData returns the connection data to send the peer in the private data
// of the connection request or reply.
func (s *nativeStream) connData() rsproto.ConnData { return s.st.ConnData() }

// connect takes the connection data of the peer, which grants it room, so
// it comes once the queue pair can send; the transport feeds the stream from
// then on.
func (s *nativeStream) connect(peer rsproto.ConnData) {
	s.mu.Lock()
	s.st.Connect(peer)
	s.mu.Unlock()
	s.t.start(s)
}

func (s *nativeStream) lock() { s.mu.Lock() }

func (s *nativeStream) unlock() {
	s.notify()
	s.mu.Unlock()
}

func (s *nativeStream) sent(id uint64, err error) {
	s.link.pending--
	s.st.Sent(id, err)
}

func (s *nativeStream) received(msg rsproto.Msg) { s.st.Received(msg) }

func (s *nativeStream) lost(err error) { s.st.LinkDown(err) }

// notify wakes the waits that may go on and a close waiting for the sends.
// The caller holds mu.
func (s *nativeStream) notify() {
	for dir, waiting := range s.waiting {
		if waiting && s.ready(direction(dir)) {
			s.waiting[dir] = false
			select {
			case s.wake[dir] <- struct{}{}:
			default:
			}
		}
	}
	if s.settled != nil && s.sendsDone() {
		close(s.settled)
		s.settled = nil
	}
}

// ready reports whether the call a wait in dir waits for would not report
// errAgain. The caller holds mu.
func (s *nativeStream) ready(dir direction) bool {
	if dir == readable {
		return s.st.Readable()
	}
	return s.st.Writable()
}

// sendsDone reports whether every send has completed and the stream holds
// none back. The caller holds mu.
func (s *nativeStream) sendsDone() bool { return s.st.SendsDone() && s.link.pending == 0 }

func (s *nativeStream) addrs() (local, remote netip.AddrPort) { return s.local, s.remote }

func (s *nativeStream) read(p []byte) (int, error) {
	v, err := s.lendRead()
	if err != nil {
		return 0, err
	}
	n := copy(p, v)
	// The bytes are read even when the stream closed meanwhile.
	s.returnRead(n)
	return n, nil
}

func (s *nativeStream) write(p []byte) (int, error) {
	v, err := s.lendWrite()
	if err != nil {
		return 0, err
	}
	n := copy(v, p)
	if err := s.commitWrite(n); err != nil {
		return 0, err
	}
	return n, nil
}

// writeError returns why nothing can be written, if anything does. The
// caller holds mu.
func (s *nativeStream) writeError() error {
	switch {
	case s.closed:
		return net.ErrClosed
	case s.writeShut:
		return errPipe
	case s.st.State()&rsproto.StateWritable == 0:
		return errConnReset
	}
	return nil
}

func (s *nativeStream) lendRead() ([]byte, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.closed {
		return nil, net.ErrClosed
	}
	v := s.st.Peek()
	if len(v) == 0 {
		if s.st.EOF() {
			return nil, io.EOF
		}
		return nil, errAgain
	}
	return v, nil
}

func (s *nativeStream) returnRead(n int) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.closed {
		return net.ErrClosed
	}
	s.st.Consume(n)
	return nil
}

func (s *nativeStream) lendWrite() ([]byte, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	if err := s.writeError(); err != nil {
		return nil, err
	}
	v := s.st.Reserve()
	if v == nil {
		return nil, errAgain
	}
	return v, nil
}

func (s *nativeStream) commitWrite(n int) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	if err := s.writeError(); err != nil {
		return err
	}
	s.st.Commit(n)
	return nil
}

func (s *nativeStream) wait(dir direction, cancel <-chan struct{}) error {
	s.mu.Lock()
	if s.closed {
		s.mu.Unlock()
		return net.ErrClosed
	}
	if s.ready(dir) {
		s.mu.Unlock()
		return nil
	}
	s.waiting[dir] = true
	s.mu.Unlock()
	// A token left by a wait that returned otherwise lets this one go on
	// early, and the caller asks again.
	select {
	case <-s.wake[dir]:
		return nil
	case <-cancel:
		return nil
	case <-s.closing:
		return net.ErrClosed
	}
}

func (s *nativeStream) closeWrite() error {
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.closed {
		return net.ErrClosed
	}
	s.writeShut = true
	s.st.CloseWrite()
	return nil
}

// close ends the connection behind the data written, waits until the sends
// have completed, at most closeTimeout, takes the link down and releases the
// memory.
func (s *nativeStream) close() error {
	s.mu.Lock()
	if s.closed {
		s.mu.Unlock()
		return nil
	}
	s.closed = true
	close(s.closing)
	s.st.Disconnect()
	settled := make(chan struct{})
	if s.sendsDone() {
		close(settled)
	} else {
		s.settled = settled
	}
	s.mu.Unlock()
	select {
	case <-settled:
	case <-time.After(closeTimeout):
	}
	s.t.down()
	s.t.release()
	return nil
}
