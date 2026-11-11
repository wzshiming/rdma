package rsproto

import (
	"fmt"
	"slices"
	"sync"
)

// NewMemLink returns the two ends of a link held in memory, which joins two
// streams as the reliable connection of two queue pairs does. Nothing moves
// on its own: Deliver carries a message, so the caller decides when, and
// from which goroutine. The two ends may be used from two goroutines, each
// driving the stream on its side.
func NewMemLink() (a, b *MemEnd) {
	l := new(memLink)
	a, b = &l.ends[0], &l.ends[1]
	a.link, a.peer = l, b
	b.link, b.peer = l, a
	return a, b
}

type memLink struct {
	mu     sync.Mutex
	ends   [2]MemEnd
	broken error // why the link carries nothing any more
	lost   error // the loss of the link Down reports
}

// MemEnd is one end of a link NewMemLink makes: the Link of the stream on
// its side.
type MemEnd struct {
	link  *memLink
	peer  *MemEnd
	mem   []Region  // registered
	sends []memSend // posted and not yet carried to the peer
	recvs int       // receives posted and not yet used up
	done  []memDone // completions of sends not yet taken

	postErr error // what the next post fails with
	sendErr error // what the next send the peer would take fails with
}

// memSend is a send posted and not yet carried.
type memSend struct {
	id   uint64
	sgl  [2]Segment
	nsge int
	msg  Msg
	addr uint64
	key  uint32
}

// memDone is the completion of a send.
type memDone struct {
	id  uint64
	err error
}

// Register makes the memory of r what the posts of this end read from and
// the writes of the peer land in under r.Key, as ibv_reg_mr does.
func (e *MemEnd) Register(r Region) {
	e.link.mu.Lock()
	defer e.link.mu.Unlock()
	e.mem = append(e.mem, r)
}

// PostWriteMsg puts the write and its message word on the link for the
// peer.
func (e *MemEnd) PostWriteMsg(id uint64, sgl []Segment, msg Msg, addr uint64, key uint32) error {
	p := memSend{id: id, nsge: len(sgl), msg: msg, addr: addr, key: key}
	copy(p.sgl[:], sgl)
	return e.post(p)
}

// PostMsg puts the message word alone on the link for the peer.
func (e *MemEnd) PostMsg(id uint64, msg Msg) error {
	return e.post(memSend{id: id, msg: msg})
}

func (e *MemEnd) post(p memSend) error {
	e.link.mu.Lock()
	defer e.link.mu.Unlock()
	if err := e.refusal(); err != nil {
		return err
	}
	if err := e.link.broken; err != nil {
		e.done = append(e.done, memDone{p.id, err})
		return nil
	}
	e.sends = append(e.sends, p)
	return nil
}

// PostRecv posts a receive for a message word of the peer.
func (e *MemEnd) PostRecv(id uint64) error {
	e.link.mu.Lock()
	defer e.link.mu.Unlock()
	if err := e.refusal(); err != nil {
		return err
	}
	e.recvs++
	return nil
}

// FailPost makes the next post of this end, of whatever kind, fail with err
// and put nothing on the link, as ibv_post_send and ibv_post_recv refuse a
// work request.
func (e *MemEnd) FailPost(err error) {
	e.link.mu.Lock()
	defer e.link.mu.Unlock()
	e.postErr = err
}

// refusal returns the error FailPost left for this post, if any, and clears
// it.
func (e *MemEnd) refusal() error {
	err := e.postErr
	e.postErr = nil
	return err
}

// FailSend makes the next send of this end fail with err when the peer would
// take it, as a send the transport gives up on, IBV_WC_RETRY_EXC_ERR among
// them, leaves the queue pair in the error state: the link carries nothing
// after it, and the sends in flight both ways, and those posted later,
// complete with err as well.
func (e *MemEnd) FailSend(err error) {
	e.link.mu.Lock()
	defer e.link.mu.Unlock()
	e.sendErr = err
}

// fail makes the link carry nothing more, every send in flight completing
// with err.
func (l *memLink) fail(err error) {
	l.broken = err
	for i := range l.ends {
		e := &l.ends[i]
		for _, p := range e.sends {
			e.done = append(e.done, memDone{p.id, err})
		}
		e.sends = e.sends[:0]
	}
}

// Break loses the link, as RDMA_CM_EVENT_DISCONNECTED tells either side:
// the link carries nothing more, the sends in flight both ways, and those
// posted later, complete with err, and Down reports err at both ends.
func (e *MemEnd) Break(err error) {
	e.link.mu.Lock()
	defer e.link.mu.Unlock()
	e.link.fail(err)
	e.link.lost = err
}

// Down returns the loss of the link Break caused, for Stream.LinkDown, once
// this end has taken the completions before it, which its stream needs to
// tell an orderly end from the loss; nil before.
func (e *MemEnd) Down() error {
	e.link.mu.Lock()
	defer e.link.mu.Unlock()
	if len(e.done) > 0 {
		return nil
	}
	return e.link.lost
}

// Deliver carries the oldest message the peer posted and this end has not
// taken into the oldest receive posted here: its bytes into the memory
// registered here and its word, returned for Stream.Received. It reports
// false when there is no message, or no receive, which leaves the message on
// the link, with those behind it, for a later Deliver to retry as the RNR
// retry of rnr_retry_count 7 does without end. Deliver writes the memory of
// this end, so the goroutine that drives its stream calls it.
func (e *MemEnd) Deliver() (Msg, bool) {
	e.link.mu.Lock()
	defer e.link.mu.Unlock()
	from := e.peer
	if len(from.sends) == 0 {
		return 0, false
	}
	if err := from.sendErr; err != nil {
		from.sendErr = nil
		e.link.fail(err)
		return 0, false
	}
	if e.recvs == 0 {
		return 0, false
	}
	p := from.sends[0]
	if err := e.write(from, p); err != nil {
		e.link.fail(err)
		return 0, false
	}
	e.recvs--
	from.sends = slices.Delete(from.sends, 0, 1)
	from.done = append(from.done, memDone{id: p.id})
	return p.msg, true
}

// write copies the bytes of p, from the memory registered at from under the
// keys of its segments, to the memory registered here under its key. A write
// of no bytes touches no memory.
func (e *MemEnd) write(from *MemEnd, p memSend) error {
	var n uint64
	for _, s := range p.sgl[:p.nsge] {
		n += uint64(s.Length)
	}
	if n == 0 {
		return nil
	}
	dst, ok := e.bytes(p.addr, n, p.key)
	if !ok {
		return fmt.Errorf("rsproto: remote access error: no %d bytes at %#x under key %#x", n, p.addr, p.key)
	}
	for _, s := range p.sgl[:p.nsge] {
		src, ok := from.bytes(s.Addr, uint64(s.Length), s.Key)
		if !ok {
			return fmt.Errorf("rsproto: local protection error: no %d bytes at %#x under key %#x", s.Length, s.Addr, s.Key)
		}
		dst = dst[copy(dst, src):]
	}
	return nil
}

// Completion takes the completion of the oldest send of this end not yet
// taken, for Stream.Sent: the id it was posted with and why it failed. It
// reports false when no send has completed: a send completes once the peer
// has taken it, and the sends of an end complete in the order of the posts.
func (e *MemEnd) Completion() (id uint64, err error, ok bool) {
	e.link.mu.Lock()
	defer e.link.mu.Unlock()
	if len(e.done) == 0 {
		return 0, nil, false
	}
	d := e.done[0]
	e.done = slices.Delete(e.done, 0, 1)
	return d.id, d.err, true
}

// bytes returns the n bytes at addr of the memory registered under key, and
// whether they all are.
func (e *MemEnd) bytes(addr, n uint64, key uint32) ([]byte, bool) {
	for _, r := range e.mem {
		if r.Key == key && addr >= r.Addr && addr-r.Addr <= uint64(len(r.Buf)) && n <= uint64(len(r.Buf))-(addr-r.Addr) {
			return r.Buf[addr-r.Addr:][:n], true
		}
	}
	return nil, false
}
