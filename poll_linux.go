//go:build linux && cgo

package rdma

import (
	"os"
	"sync"
	"syscall"

	"github.com/wzshiming/rdma/rsocket"
)

// platformNetwork returns the network built on rsockets, with one poller for
// the whole process.
func platformNetwork() network { return &rsocketNet }

var rsocketNet rsocketNetwork

// rsocketNetwork creates rsockets and polls them.
type rsocketNetwork struct{ poller }

func (*rsocketNetwork) socket(domain rsocket.Domain) (socket, error) {
	s, err := rsocket.New(domain)
	if err != nil {
		return nil, err
	}
	return &rsock{s}, nil
}

// poller is one goroutine that waits in rsocket.Poll, without a timeout
// since rpoll's own leaks, on every attached socket somebody waits for, and
// that runs the calls on the attached sockets between two polls, so that no
// rsockets call on a socket ever overlaps the poll processing it. The read
// end of a pipe is polled along with the sockets: attach, wait, run, exclude
// and detach write a byte to make the goroutine return from Poll and go
// round again. Interests are one-shot and a socket is known by its Go value,
// never by its descriptor number, which rsockets reuse. The goroutine starts
// with the first attached socket and exits with the last.
type poller struct {
	mu      sync.Mutex
	pipe    [2]int
	running bool
	sockets map[socket]*pollEntry
	calls   []*pollCall     // runs waiting for the poller
	acks    []chan struct{} // closed when the poller next stands between two polls
}

// pollEntry is an attached socket's state.
type pollEntry struct {
	waiters  []*pollWaiter
	excluded bool // out of the poll, its calls held back
}

type pollWaiter struct {
	events rsocket.Events
	ch     chan rsocket.Events
}

type pollCall struct {
	s    socket
	f    func()
	done chan bool // whether f ran
}

// events returns the union of the waiters' interests.
func (e *pollEntry) events() rsocket.Events {
	var ev rsocket.Events
	for _, w := range e.waiters {
		ev |= w.events
	}
	return ev
}

// deliver hands re to every waiter it concerns and drops them.
func (e *pollEntry) deliver(re rsocket.Events) {
	kept := e.waiters[:0]
	for _, w := range e.waiters {
		if re&(w.events|rsocket.PollErr|rsocket.PollHup|rsocket.PollNval) != 0 {
			w.ch <- re
		} else {
			kept = append(kept, w)
		}
	}
	e.waiters = kept
}

func (e *pollEntry) remove(w *pollWaiter) {
	for i, x := range e.waiters {
		if x == w {
			e.waiters = append(e.waiters[:i], e.waiters[i+1:]...)
			return
		}
	}
}

func (p *poller) attach(s socket) error {
	p.mu.Lock()
	defer p.mu.Unlock()
	if !p.running {
		if err := syscall.Pipe2(p.pipe[:], syscall.O_NONBLOCK|syscall.O_CLOEXEC); err != nil {
			return os.NewSyscallError("pipe2", err)
		}
		if p.sockets == nil {
			p.sockets = make(map[socket]*pollEntry)
		}
		p.running = true
		go p.loop()
	}
	p.sockets[s] = &pollEntry{}
	return nil
}

func (p *poller) run(s socket, f func()) bool {
	p.mu.Lock()
	if p.sockets[s] == nil {
		p.mu.Unlock()
		return false
	}
	c := &pollCall{s: s, f: f, done: make(chan bool, 1)}
	p.calls = append(p.calls, c)
	p.wake()
	p.mu.Unlock()
	return <-c.done
}

func (p *poller) exclude(s socket) {
	p.mu.Lock()
	e := p.sockets[s]
	if e == nil {
		p.mu.Unlock()
		return
	}
	e.excluded = true
	ack := p.quiesce()
	p.mu.Unlock()
	<-ack
}

func (p *poller) include(s socket) {
	p.mu.Lock()
	defer p.mu.Unlock()
	if e := p.sockets[s]; e != nil {
		e.excluded = false
		p.wake()
	}
}

func (p *poller) wait(s socket, events rsocket.Events) (<-chan rsocket.Events, func()) {
	ch := make(chan rsocket.Events, 1)
	p.mu.Lock()
	defer p.mu.Unlock()
	e := p.sockets[s]
	if e == nil {
		ch <- rsocket.PollNval
		return ch, func() {}
	}
	w := &pollWaiter{events: events, ch: ch}
	before := e.events()
	e.waiters = append(e.waiters, w)
	if before|events != before {
		p.wake()
	}
	return ch, func() {
		p.mu.Lock()
		defer p.mu.Unlock()
		if e := p.sockets[s]; e != nil {
			e.remove(w)
		}
	}
}

func (p *poller) detach(s socket) {
	p.mu.Lock()
	if _, ok := p.sockets[s]; !ok {
		p.mu.Unlock()
		return
	}
	delete(p.sockets, s)
	ack := p.quiesce()
	p.mu.Unlock()
	<-ack
}

// quiesce returns a channel closed once the poller next stands between two
// polls, having taken in every change made so far, or at once when it is
// not running. The caller holds mu.
func (p *poller) quiesce() <-chan struct{} {
	ack := make(chan struct{})
	if !p.running {
		close(ack)
		return ack
	}
	p.acks = append(p.acks, ack)
	p.wake()
	return ack
}

// wake makes the goroutine return from Poll. The caller holds mu. A full
// pipe already wakes it.
func (p *poller) wake() { syscall.Write(p.pipe[1], []byte{0}) }

// drain empties the pipe. The caller holds mu.
func (p *poller) drain() {
	var buf [64]byte
	for {
		if n, err := syscall.Read(p.pipe[0], buf[:]); n < len(buf) || err != nil {
			return
		}
	}
}

// loop is the poller goroutine: between two polls it acknowledges the
// changes made since the last one, runs the calls of the sockets in the
// poll, and rebuilds the descriptor set.
func (p *poller) loop() {
	var fds []rsocket.PollFD
	var polled []socket
	for {
		p.mu.Lock()
		acks := p.acks
		p.acks = nil
		if len(p.sockets) == 0 {
			p.running = false
			syscall.Close(p.pipe[0])
			syscall.Close(p.pipe[1])
			calls := p.calls
			p.calls = nil
			p.mu.Unlock()
			for _, ack := range acks {
				close(ack)
			}
			for _, c := range calls {
				c.done <- false
			}
			return
		}
		var ready, held []*pollCall
		for _, c := range p.calls {
			switch e := p.sockets[c.s]; {
			case e == nil:
				c.done <- false
			case e.excluded:
				held = append(held, c)
			default:
				ready = append(ready, c)
			}
		}
		p.calls = held
		p.mu.Unlock()
		for _, ack := range acks {
			close(ack)
		}
		for _, c := range ready {
			c.f()
			c.done <- true
		}

		p.mu.Lock()
		fds = append(fds[:0], rsocket.PollFD{Fd: p.pipe[0], Events: rsocket.PollIn})
		polled = polled[:0]
		for s, e := range p.sockets {
			if ev := e.events(); ev != 0 && !e.excluded {
				fds = append(fds, rsocket.PollFD{Fd: s.Descriptor(), Events: ev})
				polled = append(polled, s)
			}
		}
		p.mu.Unlock()
		_, err := rsocket.Poll(fds, -1)
		p.mu.Lock()
		if fds[0].Revents != 0 {
			p.drain()
		}
		for i, s := range polled {
			re := fds[i+1].Revents
			if err != nil {
				re = rsocket.PollErr
			}
			if e := p.sockets[s]; e != nil && re != 0 {
				e.deliver(re)
			}
		}
		p.mu.Unlock()
	}
}
