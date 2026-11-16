//go:build linux && !android && (amd64 || arm64 || loong64 || ppc64le || riscv64 || s390x)

package rdma

import (
	"net/netip"
	"os"
	"runtime"
	"sync"
	"syscall"
	"testing"
	"time"

	"github.com/wzshiming/rdma/internal/rsocket"
)

// rsocketNet is the network of the librdmacm engine, with one poller for the
// whole process.
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

// rsock adapts *rsocket.Socket to the socket interface, whose Accept
// returns the interface.
type rsock struct{ *rsocket.Socket }

func (s *rsock) Accept() (socket, netip.AddrPort, error) {
	c, peer, err := s.Socket.Accept()
	if err != nil {
		return nil, netip.AddrPort{}, err
	}
	return &rsock{c}, peer, nil
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

// pipeSocket is a socket whose descriptor is the read end of a pipe, so the
// poller's rpoll sees a plain descriptor; the rest of the interface is never
// called.
type pipeSocket struct {
	socket
	r, w int
}

func newPipeSocket(t *testing.T) *pipeSocket {
	t.Helper()
	var p [2]int
	if err := syscall.Pipe2(p[:], syscall.O_NONBLOCK|syscall.O_CLOEXEC); err != nil {
		t.Fatalf("Pipe2() error: %v", err)
	}
	t.Cleanup(func() {
		syscall.Close(p[0])
		syscall.Close(p[1])
	})
	return &pipeSocket{r: p[0], w: p[1]}
}

func (s *pipeSocket) Descriptor() int { return s.r }

// fill makes the socket readable.
func (s *pipeSocket) fill(t *testing.T) {
	t.Helper()
	if _, err := syscall.Write(s.w, []byte{1}); err != nil {
		t.Fatalf("Write() to the pipe error: %v", err)
	}
}

// expectEvents waits for a delivery on ready and checks it carries want.
func expectEvents(t *testing.T, ready <-chan rsocket.Events, want rsocket.Events) {
	t.Helper()
	select {
	case got := <-ready:
		if got&want == 0 {
			t.Fatalf("delivered %v, want %v", got, want)
		}
	case <-time.After(5 * time.Second):
		t.Fatal("no readiness delivered")
	}
}

// expectSilence checks that nothing is delivered on ready for a while.
func expectSilence(t *testing.T, ready <-chan rsocket.Events) {
	t.Helper()
	select {
	case got := <-ready:
		t.Fatalf("delivered %v, want nothing", got)
	case <-time.After(100 * time.Millisecond):
	}
}

// waitForGoroutines waits until the goroutine count is back to n.
func waitForGoroutines(t *testing.T, n int) {
	t.Helper()
	deadline := time.Now().Add(5 * time.Second)
	for runtime.NumGoroutine() > n && time.Now().Before(deadline) {
		time.Sleep(5 * time.Millisecond)
	}
	if got := runtime.NumGoroutine(); got > n {
		t.Errorf("goroutines = %d, want %d: the poller did not exit", got, n)
	}
}

func TestPollerDeliversReadinessOnce(t *testing.T) {
	p := &poller{}
	s := newPipeSocket(t)
	if err := p.attach(s); err != nil {
		t.Fatalf("attach() error: %v", err)
	}
	ready, _ := p.wait(s, rsocket.PollIn)
	expectSilence(t, ready)
	s.fill(t)
	expectEvents(t, ready, rsocket.PollIn)
	// The interest is spent: a new wait is needed for the next delivery, and
	// the descriptor is still readable, so it comes at once.
	expectSilence(t, ready)
	again, _ := p.wait(s, rsocket.PollIn)
	expectEvents(t, again, rsocket.PollIn)
	p.detach(s)
}

func TestPollerServesTwoWaitersOnOneSocket(t *testing.T) {
	p := &poller{}
	s := newPipeSocket(t)
	if err := p.attach(s); err != nil {
		t.Fatalf("attach() error: %v", err)
	}
	in1, _ := p.wait(s, rsocket.PollIn)
	in2, _ := p.wait(s, rsocket.PollIn)
	s.fill(t)
	expectEvents(t, in1, rsocket.PollIn)
	expectEvents(t, in2, rsocket.PollIn)
	p.detach(s)
}

func TestPollerCancelWithdrawsTheInterest(t *testing.T) {
	p := &poller{}
	s := newPipeSocket(t)
	if err := p.attach(s); err != nil {
		t.Fatalf("attach() error: %v", err)
	}
	ready, cancel := p.wait(s, rsocket.PollIn)
	cancel()
	s.fill(t)
	expectSilence(t, ready)
	p.detach(s)
}

func TestPollerDetachReturnsOnceTheDescriptorIsOut(t *testing.T) {
	p := &poller{}
	s := newPipeSocket(t)
	if err := p.attach(s); err != nil {
		t.Fatalf("attach() error: %v", err)
	}
	ready, _ := p.wait(s, rsocket.PollIn)
	done := make(chan struct{})
	go func() {
		p.detach(s)
		close(done)
	}()
	select {
	case <-done:
	case <-time.After(5 * time.Second):
		t.Fatal("detach() did not return while the poller waited on the socket")
	}
	// The poller no longer knows the socket: readiness is not delivered to
	// the old waiter and a new wait is answered at once.
	s.fill(t)
	expectSilence(t, ready)
	late, _ := p.wait(s, rsocket.PollIn)
	expectEvents(t, late, rsocket.PollNval)
}

// settledGoroutines returns the goroutine count once it has stopped changing,
// so that a poller of an earlier test still on its way out is not counted.
func settledGoroutines() int {
	n := runtime.NumGoroutine()
	for same, i := 0, 0; same < 3 && i < 200; i++ {
		time.Sleep(10 * time.Millisecond)
		if m := runtime.NumGoroutine(); m == n {
			same++
		} else {
			n, same = m, 0
		}
	}
	return n
}

func TestPollerExitsWithTheLastSocket(t *testing.T) {
	p := &poller{}
	before := settledGoroutines()
	a, b := newPipeSocket(t), newPipeSocket(t)
	for _, s := range []*pipeSocket{a, b} {
		if err := p.attach(s); err != nil {
			t.Fatalf("attach() error: %v", err)
		}
	}
	if runtime.NumGoroutine() != before+1 {
		t.Fatalf("goroutines = %d after attaching two sockets, want %d: one poller", runtime.NumGoroutine(), before+1)
	}
	p.detach(a)
	if runtime.NumGoroutine() != before+1 {
		t.Fatalf("goroutines = %d with one socket attached, want %d: the poller stays", runtime.NumGoroutine(), before+1)
	}
	p.detach(b)
	waitForGoroutines(t, before)
	// It comes back for the next socket.
	if err := p.attach(a); err != nil {
		t.Fatalf("attach() after the poller exited error: %v", err)
	}
	ready, _ := p.wait(a, rsocket.PollIn)
	a.fill(t)
	expectEvents(t, ready, rsocket.PollIn)
	p.detach(a)
	waitForGoroutines(t, before)
}

// TestPollerRunsCallsOnItsGoroutine checks that run executes its functions
// one at a time on the poller goroutine, between two polls, however many
// goroutines ask at once and even while the poller waits on a socket, and
// that it runs nothing for a socket that is not attached.
func TestPollerRunsCallsOnItsGoroutine(t *testing.T) {
	p := &poller{}
	s := newPipeSocket(t)
	if err := p.attach(s); err != nil {
		t.Fatalf("attach() error: %v", err)
	}
	ready, _ := p.wait(s, rsocket.PollIn) // keeps the poller in Poll
	// Plain increments would be a data race under -race unless every call
	// runs on the same goroutine.
	count := 0
	var wg sync.WaitGroup
	for range 100 {
		wg.Add(1)
		go func() {
			defer wg.Done()
			if !p.run(s, func() { count++ }) {
				t.Error("run() on an attached socket reported it did not run")
			}
		}()
	}
	done := make(chan struct{})
	go func() {
		wg.Wait()
		close(done)
	}()
	select {
	case <-done:
	case <-time.After(5 * time.Second):
		t.Fatal("run() did not return while the poller waited on a socket")
	}
	if count != 100 {
		t.Fatalf("count = %d after 100 calls, want 100", count)
	}
	expectSilence(t, ready)
	p.detach(s)
	if p.run(s, func() { count++ }) || count != 100 {
		t.Fatalf("run() on a detached socket ran the function (count %d)", count)
	}
}

// TestPollerExcludeKeepsTheSocketOutOfThePoll checks that between exclude
// and include the poller neither polls the socket nor runs calls on it, that
// its waiters and calls survive until include, and that exclude returns at
// once for a socket nobody waits on.
func TestPollerExcludeKeepsTheSocketOutOfThePoll(t *testing.T) {
	p := &poller{}
	s := newPipeSocket(t)
	if err := p.attach(s); err != nil {
		t.Fatalf("attach() error: %v", err)
	}
	ready, _ := p.wait(s, rsocket.PollIn)
	excluded := make(chan struct{})
	go func() {
		p.exclude(s)
		close(excluded)
	}()
	select {
	case <-excluded:
	case <-time.After(5 * time.Second):
		t.Fatal("exclude() did not return while the poller waited on the socket")
	}
	s.fill(t)
	expectSilence(t, ready)
	ran := make(chan bool, 1)
	go func() { ran <- p.run(s, func() {}) }()
	select {
	case <-ran:
		t.Fatal("run() on an excluded socket ran before include()")
	case <-time.After(100 * time.Millisecond):
	}
	p.include(s)
	expectEvents(t, ready, rsocket.PollIn)
	select {
	case ok := <-ran:
		if !ok {
			t.Fatal("run() deferred by exclude() reported it did not run")
		}
	case <-time.After(5 * time.Second):
		t.Fatal("run() deferred by exclude() did not return after include()")
	}
	// Nobody waits on the socket now, so it is not polled and exclude has
	// nothing to wait for.
	start := time.Now()
	p.exclude(s)
	p.include(s)
	if elapsed := time.Since(start); elapsed > 50*time.Millisecond {
		t.Errorf("exclude() and include() of an unpolled socket took %v", elapsed)
	}
	p.detach(s)
}
