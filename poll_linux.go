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

// poller waits for readiness on every attached socket somebody waits for
// from one goroutine blocked in rsocket.Poll without a timeout, which rpoll
// handles without the leak its own timeout has. The read end of a pipe is
// polled along with the sockets: attach, wait and detach write a byte to
// make the goroutine rebuild its descriptor set. Interests are one-shot and
// a socket is known by its Go value, never by its descriptor number, which
// rsockets reuse. The goroutine starts with the first attached socket and
// exits with the last.
type poller struct {
	mu       sync.Mutex
	pipe     [2]int
	running  bool
	sockets  map[socket]*pollEntry
	detached []chan struct{} // acknowledgements owed once the descriptor set is rebuilt
}

// pollEntry is an attached socket's waiters.
type pollEntry struct {
	waiters []*pollWaiter
}

type pollWaiter struct {
	events rsocket.Events
	ch     chan rsocket.Events
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
		go p.run()
	}
	p.sockets[s] = &pollEntry{}
	return nil
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
	ack := make(chan struct{})
	p.detached = append(p.detached, ack)
	p.wake()
	p.mu.Unlock()
	<-ack
}

// wake makes the goroutine return from Poll and rebuild its descriptor set.
// The caller holds mu. A full pipe already wakes it.
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

// run is the poller goroutine.
func (p *poller) run() {
	var fds []rsocket.PollFD
	var polled []socket
	for {
		p.mu.Lock()
		acks := p.detached
		p.detached = nil
		if len(p.sockets) == 0 {
			p.running = false
			syscall.Close(p.pipe[0])
			syscall.Close(p.pipe[1])
			p.mu.Unlock()
			for _, ack := range acks {
				close(ack)
			}
			return
		}
		fds = append(fds[:0], rsocket.PollFD{Fd: p.pipe[0], Events: rsocket.PollIn})
		polled = polled[:0]
		for s, e := range p.sockets {
			if ev := e.events(); ev != 0 {
				fds = append(fds, rsocket.PollFD{Fd: s.Descriptor(), Events: ev})
				polled = append(polled, s)
			}
		}
		p.mu.Unlock()
		// The set no longer holds the detached sockets, and the Poll that
		// may have held them has returned.
		for _, ack := range acks {
			close(ack)
		}
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
