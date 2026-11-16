package rdma

import (
	"io"
	"net"
	"net/netip"
	"sync"
	"sync/atomic"
	"time"

	"github.com/wzshiming/rdma/internal/rsproto"
)

// closeTimeout bounds how long close waits for the data posted and the end
// of the connection to reach the peer.
const closeTimeout = time.Second

// clockZero is where the clock of the polls starts.
var clockZero = time.Now()

// monotonic reads the clock of the polls, the monotonic one.
func monotonic() time.Duration { return time.Since(clockZero) }

// The polls of the waits in one direction back off while they find nothing:
// after pollMisses misses in a row the next pollPause waits do not poll, and
// each further miss in a row doubles that pause, pollDoublings times at
// most; a poll that finds the stream ready ends the back-off.
const (
	pollMisses    = 2
	pollPause     = 16
	pollDoublings = 6
)

// pollPace paces the polls of the waits in one direction.
type pollPace struct {
	misses int // polls in a row that found nothing
	skip   int // waits left that do not poll
}

// due reports whether the wait about to come polls, and counts it against
// the pause when it does not.
func (p *pollPace) due() bool {
	if p.skip > 0 {
		p.skip--
		return false
	}
	return true
}

// found ends the back-off: a poll found the stream ready.
func (p *pollPace) found() { p.misses = 0 }

// missed counts a poll that found nothing, which pauses the polls after
// pollMisses in a row.
func (p *pollPace) missed() {
	if p.misses++; p.misses >= pollMisses {
		p.skip = pollPause << min(p.misses-pollMisses, pollDoublings)
	}
}

// transport carries a native stream: the queue pair of a connection, or a
// link held in memory in tests. The stream posts through its rsproto.Link,
// from inside its own calls, and has the transport process the completions
// of its queue: feed the stream what has become of the posts and what has
// arrived, through the linkEvents start hands it.
type transport interface {
	rsproto.Link
	// start has the transport feed ev the events of the link from then on,
	// in the order package rsproto asks for: sent for the completion of each
	// send, received for each message word that arrives, and lost once, for
	// the loss of the link, after the completions before it. Each batch of
	// events comes with ev's lock held. The loss comes in the background, a
	// completion as the transport processes it.
	start(ev linkEvents)
	// process feeds ev the completions that are ready, without waiting for
	// any. Its caller holds no lock of the stream; while another goroutine
	// processes, the call leaves the work to it.
	process()
	// expect arms the transport for the completions of the kinds k, which ev
	// has come to wait for, unless it is armed for them, and then processes,
	// as what came before the arm raises nothing: those that come later are
	// processed in the background, and the transport arms again after them
	// for as long as ev waits for any. Its caller holds no lock of the stream.
	expect(k interest)
	// down takes the link down, which the peer learns as its loss, and
	// returns once the transport feeds the stream nothing more and touches
	// its memory no more. It may come without start.
	down()
	// release frees the memory the stream was built on; it comes after down.
	release()
}

// linkEvents is the native stream as the transport under it sees it. The
// transport takes the lock, feeds the events and lets the lock go, which
// wakes whoever the events let go on; it calls nothing else in between but
// waitsFor.
type linkEvents interface {
	lock()
	unlock()
	sent(id uint64, err error)
	received(msg rsproto.Msg)
	lost(err error)
	// waitsFor returns the kinds of completions the stream waits for, which
	// the transport keeps processing in the background.
	waitsFor() interest
}

// interest names the kinds of completions a native stream waits for, which
// a transport may keep apart.
type interest uint8

const (
	arrivals        interest = 1 << iota // of receives: what the peer sent
	sendCompletions                      // of sends: the stream's own posts

	anyCompletion = arrivals | sendCompletions
)

// waitsOn names the completions a wait in each direction waits for: data
// arrives, and room comes back as sends complete and as credits and grants
// arrive.
var waitsOn = [...]interest{readable: arrivals, writable: arrivals | sendCompletions}

// completionQueue is the queue of completions of a transport as its
// processor drains and arms it.
type completionQueue interface {
	// drain feeds ev, a batch at a time under its lock, what the queue holds,
	// and the loss of the link after it once the link is lost, and returns
	// what ev waits for at the last batch; known is false when it fed none.
	drain(ev linkEvents) (k interest, known bool)
	// arm has the queue raise an event for its next completion of the kinds
	// k and returns the kinds it armed, none when it failed.
	arm(ev linkEvents, k interest) interest
}

// processor processes the completion queue of a transport for its stream,
// one goroutine at a time, whichever goroutines ask: it drains the queue
// into the stream and arms it for what the stream waits for. A goroutine
// that asks while another processes leaves its ask to that one, which serves
// it before it stops, so that no ask blocks.
type processor struct {
	queued atomic.Int32  // asks not yet served: the ask that raises it from zero serves them all
	asks   atomic.Uint32 // what they ask for: askDrain, the kinds to arm for, the kinds spent

	mu      sync.Mutex // held while serving; stop takes it
	q       completionQueue
	ev      linkEvents // nil until start
	armed   interest   // the kinds the queue raises an event for
	stopped bool
}

const (
	askDrain   = 1 << 8 // a drain
	spentShift = 4      // where the kinds an event of the queue spent sit, above those to arm for
)

// start has p process q for ev from then on.
func (p *processor) start(q completionQueue, ev linkEvents) {
	p.mu.Lock()
	p.q, p.ev = q, ev
	p.mu.Unlock()
}

// process drains the queue.
func (p *processor) process() { p.ask(askDrain) }

// expect arms the queue for the kinds k unless it is armed for them, and
// then drains it.
func (p *processor) expect(k interest) { p.ask(uint32(k)) }

// spent takes the event the queue raised for the kinds k, which disarmed it
// for them: it drains the queue and arms it again for what the stream still
// waits for.
func (p *processor) spent(k interest) { p.ask(uint32(k) << spentShift) }

// stop ends the processing: once it returns, the queue is drained and armed
// no more.
func (p *processor) stop() {
	p.mu.Lock()
	p.stopped = true
	p.mu.Unlock()
}

func (p *processor) ask(a uint32) {
	p.asks.Or(a)
	if p.queued.Add(1) != 1 {
		return
	}
	for {
		n := p.queued.Load()
		p.serve(p.asks.Swap(0))
		if p.queued.Add(-n) == 0 {
			return
		}
	}
}

// serve does what the asks summed up in a want: a drain, asked for or after
// an event, and an arm for the kinds expected and for those the stream waits
// for once a drain fed it, each arm followed by a drain.
func (p *processor) serve(a uint32) {
	p.mu.Lock()
	defer p.mu.Unlock()
	if p.ev == nil || p.stopped {
		return
	}
	expect, spent := interest(a)&anyCompletion, interest(a>>spentShift)&anyCompletion
	p.armed &^= spent
	var want interest
	if a&askDrain != 0 || spent != 0 {
		var known bool
		if want, known = p.q.drain(p.ev); !known && spent != 0 {
			p.ev.lock()
			want = p.ev.waitsFor()
			p.ev.unlock()
		}
	}
	for need := (want | expect) &^ p.armed; need != 0; need = want &^ p.armed {
		armed := p.q.arm(p.ev, need)
		if armed == 0 {
			return
		}
		p.armed |= armed
		// What came before the arm raises no event.
		var known bool
		if want, known = p.q.drain(p.ev); !known {
			return
		}
	}
}

// nativeStream is a stream of the native engine: one rsproto.Stream that the
// front end drives through the calls of the stream interface and its
// transport with the completions it processes, at a wait and in the
// background while the stream waits for them.
type nativeStream struct {
	t             transport
	local, remote netip.AddrPort
	closing       chan struct{}    // closed by close, waking every wait
	wake          [2]chan struct{} // by direction, one slot for the token that lets a wait go on

	mu        sync.Mutex
	st        *rsproto.Stream
	link      sendCounter
	waiting   [2]bool // by direction, a wait sleeps on wake
	fed       bool    // events came since the lock was taken
	rview     bool    // a read view is out
	wview     bool    // a write view is out
	writeShut bool    // closeWrite was called
	closed    bool
	settled   chan struct{} // closed once the sends of a closing stream are done
	unfreed   bool          // the link is down and the memory awaits the views out

	poller *streamPoller // the polls of the waits, nil when they do not poll; set before connect
}

// streamPoller holds what the polls of the waits of a stream go by.
type streamPoller struct {
	time  time.Duration        // how long a wait polls before it declares
	now   func() time.Duration // the clock the polls go by
	paces [2]pollPace          // by direction, the waits', which come one at a time
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

// pollFor has the waits of s poll for d before they declare, and not at all
// for zero. It is called before connect.
func (s *nativeStream) pollFor(d time.Duration) {
	if d > 0 {
		s.poller = &streamPoller{time: d, now: monotonic}
	}
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
	if s.fed {
		s.fed = false
		s.notify()
	}
	s.mu.Unlock()
}

func (s *nativeStream) sent(id uint64, err error) {
	s.fed = true
	s.link.pending--
	s.st.Sent(id, err)
}

func (s *nativeStream) received(msg rsproto.Msg) {
	s.fed = true
	s.st.Received(msg)
}

func (s *nativeStream) lost(err error) {
	s.fed = true
	s.st.LinkDown(err)
}

// waitsFor returns what the waits declared, the completions of the sends a
// closing stream waits for, and those of the control messages in flight
// while another is held for a slot, which has to go out whether or not a
// call comes. The caller holds mu.
func (s *nativeStream) waitsFor() interest {
	var k interest
	for dir, waiting := range s.waiting {
		if waiting {
			k |= waitsOn[dir]
		}
	}
	if s.settled != nil || s.st.ControlHeld() {
		k |= sendCompletions
	}
	return k
}

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
			if err := s.st.Err(); err != nil && err != io.EOF {
				return nil, errConnReset
			}
			return nil, io.EOF
		}
		return nil, errAgain
	}
	s.rview = true
	return v, nil
}

func (s *nativeStream) returnRead(n int) error {
	s.mu.Lock()
	s.rview = false
	if s.closed {
		s.freeIdle()
		s.mu.Unlock()
		return net.ErrClosed
	}
	s.st.Consume(n)
	held := s.st.ControlHeld()
	s.mu.Unlock()
	if held {
		s.t.expect(sendCompletions)
	}
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
	s.wview = true
	return v, nil
}

func (s *nativeStream) commitWrite(n int) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.wview = false
	if err := s.writeError(); err != nil {
		s.freeIdle()
		return err
	}
	if s.st.Commit(n) != nil {
		return errConnReset
	}
	return nil
}

// wait polls the stream for up to the poll time, as rs_get_comp polls
// before it blocks, or, when the stream does not poll or while the polls in
// dir back off, has the transport process what is ready once, and unless
// that makes the stream ready in dir, declares what it waits for and sleeps
// until the transport, processing in the background, wakes it.
func (s *nativeStream) wait(dir direction, cancel <-chan struct{}) error {
	if p := s.poller; p != nil && p.paces[dir].due() {
		if over, err := s.poll(dir, cancel); over {
			return err
		}
	} else {
		s.t.process()
	}
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
	s.t.expect(waitsOn[dir])
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

// poll has the transport process what is ready and looks again and again,
// for up to the poll time, until the stream is ready in dir, closed or
// cancel is closed: it reports whether one of them ended the wait, with
// net.ErrClosed for the stream closed. Ready at the first look, the stream
// is no find of the poll's.
func (s *nativeStream) poll(dir direction, cancel <-chan struct{}) (over bool, err error) {
	p := s.poller
	end := p.now() + p.time
	for looked := false; ; looked = true {
		s.t.process()
		s.mu.Lock()
		if s.closed {
			s.mu.Unlock()
			return true, net.ErrClosed
		}
		ready := s.ready(dir)
		s.mu.Unlock()
		if ready {
			if looked {
				p.paces[dir].found()
			}
			return true, nil
		}
		select {
		case <-cancel:
			return true, nil
		default:
		}
		if p.now() >= end {
			p.paces[dir].missed()
			return false, nil
		}
	}
}

func (s *nativeStream) closeWrite() error {
	s.mu.Lock()
	if s.closed {
		s.mu.Unlock()
		return net.ErrClosed
	}
	s.writeShut = true
	s.st.CloseWrite()
	held := s.st.ControlHeld()
	s.mu.Unlock()
	if held {
		s.t.expect(sendCompletions)
	}
	return nil
}

// close ends the connection behind the data written, waits until the sends
// have completed, at most closeTimeout, takes the link down and releases the
// memory once no view of it is out.
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
	done := s.sendsDone()
	if done {
		close(settled)
	} else {
		s.settled = settled
	}
	s.mu.Unlock()
	if !done {
		s.t.expect(sendCompletions)
	}
	select {
	case <-settled:
	case <-time.After(closeTimeout):
	}
	s.t.down()
	s.mu.Lock()
	s.unfreed = true
	s.freeIdle()
	s.mu.Unlock()
	return nil
}

// freeIdle releases the memory once the link is down and no view is out.
// The caller holds mu.
func (s *nativeStream) freeIdle() {
	if s.unfreed && !s.rview && !s.wview {
		s.unfreed = false
		s.t.release()
	}
}
