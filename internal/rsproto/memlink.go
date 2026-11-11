package rsproto

import (
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
	mu   sync.Mutex
	ends [2]MemEnd
}

// MemEnd is one end of a link NewMemLink makes: the Link of the stream on
// its side.
type MemEnd struct {
	link  *memLink
	peer  *MemEnd
	mem   []Region  // registered
	sends []memSend // posted and not yet carried to the peer
	done  []uint64  // the ids of sends carried and not yet taken by Completion
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
	e.link.mu.Lock()
	defer e.link.mu.Unlock()
	e.sends = append(e.sends, p)
	return nil
}

// PostMsg puts the message word alone on the link for the peer.
func (e *MemEnd) PostMsg(id uint64, msg Msg) error {
	e.link.mu.Lock()
	defer e.link.mu.Unlock()
	e.sends = append(e.sends, memSend{id: id, msg: msg})
	return nil
}

// PostRecv posts a receive for a message word of the peer.
func (e *MemEnd) PostRecv(id uint64) error { return nil }

// Deliver carries the oldest message the peer posted and this end has not
// taken: its bytes into the memory registered here and its word, returned
// for Stream.Received. It reports false when there is none. Deliver writes
// the memory of this end, so the goroutine that drives its stream calls it.
func (e *MemEnd) Deliver() (Msg, bool) {
	e.link.mu.Lock()
	defer e.link.mu.Unlock()
	from := e.peer
	if len(from.sends) == 0 {
		return 0, false
	}
	p := from.sends[0]
	from.sends = slices.Delete(from.sends, 0, 1)
	var n uint64
	for _, s := range p.sgl[:p.nsge] {
		n += uint64(s.Length)
	}
	dst := e.bytes(p.addr, n, p.key)
	for _, s := range p.sgl[:p.nsge] {
		dst = dst[copy(dst, from.bytes(s.Addr, uint64(s.Length), s.Key)):]
	}
	from.done = append(from.done, p.id)
	return p.msg, true
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
	id = e.done[0]
	e.done = slices.Delete(e.done, 0, 1)
	return id, nil, true
}

// bytes returns the n bytes at addr of the memory registered under key.
func (e *MemEnd) bytes(addr, n uint64, key uint32) []byte {
	for _, r := range e.mem {
		if r.Key == key {
			return r.Buf[addr-r.Addr:][:n]
		}
	}
	return nil
}
