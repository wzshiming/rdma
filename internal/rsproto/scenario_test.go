package rsproto_test

import (
	"bytes"
	"errors"
	"io"
	"math/rand/v2"
	"runtime"
	"strings"
	"sync"
	"testing"

	"github.com/wzshiming/rdma/internal/rsproto"
)

// side is one of two streams a MemLink joins: the stream, its end of the
// link, the memory it was built on, what reached it and what it sent.
type side struct {
	s       *rsproto.Stream
	l       *rsproto.MemEnd
	c       rsproto.Config
	arrived [8]int // messages delivered, by op
	limit   uint16 // the credit limit of the last RS_OP_SGL delivered
	sent    int    // data messages committed
}

// smallConfig describes memory and queues as small as rsockets allows, from
// base on: a receive queue of RS_QP_MIN_SIZE, a send queue of twice that, so
// that a writer runs out of the peer's credits before its own send queue
// entries, and buffers of two RS_SNDLOWAT, of which rs_can_send wants one
// free, the receive buffer granted in halves of a dozen messages or so.
func smallConfig(base uint64) rsproto.Config {
	return rsproto.Config{
		SendBuffer:       rsproto.Region{Buf: make([]byte, 2*rsproto.SndLowat), Addr: base, Key: 1},
		ReceiveBuffer:    rsproto.Region{Buf: make([]byte, 2*rsproto.SndLowat), Addr: base + 0x10000, Key: 2},
		GrantTable:       rsproto.Region{Buf: make([]byte, rsproto.SGLSize*rsproto.SGESize), Addr: base + 0x20000, Key: 3},
		ControlBuffer:    rsproto.Region{Buf: make([]byte, rsproto.QPCtrlSize*rsproto.SGESize), Addr: base + 0x30000, Key: 4},
		SendQueueSize:    2 * rsproto.QPMinSize,
		ReceiveQueueSize: rsproto.QPMinSize,
	}
}

// join builds a stream on each of ca and cb, on the two ends of a MemLink
// with its memory registered there, and connects them as rs_accept and
// rs_do_connect do, the grants of the first rs_process_cq carried and
// completed.
func join(ca, cb rsproto.Config) (a, b *side) {
	la, lb := rsproto.NewMemLink()
	a, b = newSide(la, ca), newSide(lb, cb)
	a.s.Connect(b.s.ConnData())
	b.s.Connect(a.s.ConnData())
	pump(a, b)
	return a, b
}

func newSide(l *rsproto.MemEnd, c rsproto.Config) *side {
	for _, r := range []rsproto.Region{c.SendBuffer, c.ReceiveBuffer, c.GrantTable, c.ControlBuffer} {
		l.Register(r)
	}
	return &side{s: rsproto.NewStream(l, c), l: l, c: c}
}

// deliver hands the stream every message the link carries to it now and
// reports whether there was any.
func (x *side) deliver() bool {
	moved := false
	for msg, ok := x.l.Deliver(); ok; msg, ok = x.l.Deliver() {
		x.arrived[msg.Op()]++
		if msg.Op() == rsproto.OpSGL {
			x.limit = uint16(msg.Data())
		}
		x.s.Received(msg)
		moved = true
	}
	return moved
}

// complete hands the stream the completion of every send of its that has
// completed and reports whether there was any.
func (x *side) complete() bool {
	moved := false
	for id, err, ok := x.l.Completion(); ok; id, err, ok = x.l.Completion() {
		x.s.Sent(id, err)
		moved = true
	}
	return moved
}

// pump carries messages and completions both ways until none is left and
// reports whether there was any.
func pump(a, b *side) bool {
	moved := false
	for a.deliver() || b.deliver() || a.complete() || b.complete() {
		moved = true
	}
	return moved
}

// send writes p through the views x lends, failing t unless all of it fits.
func send(t *testing.T, x *side, p string) {
	t.Helper()
	for p != "" {
		v := x.s.Reserve()
		if v == nil {
			t.Fatalf("no room to write %q", p)
		}
		n := copy(v, p)
		if err := x.s.Commit(n); err != nil {
			t.Fatalf("Commit(%d) = %v", n, err)
		}
		p = p[n:]
	}
}

// recv reads what has arrived at x through the views it lends.
func recv(x *side) string {
	var b []byte
	for v := x.s.Peek(); len(v) > 0; v = x.s.Peek() {
		b = append(b, v...)
		x.s.Consume(len(v))
	}
	return string(b)
}

// flow is one way of TestStreamsCarryBytesIntact: what w writes and r reads.
type flow struct {
	w, r   *side
	sent   []byte
	got    []byte
	stalls int // times w had no room with its credits used up
	wraps  int // views r lent from the start of its receive buffer
}

// Two streams joined by the link carry a byte stream each way intact through
// memory and queues as small as rsockets allows: the writers keep running
// out of credits, the readers grant the halves of their receive buffers over
// and over in turn, rs_write_data cuts messages at the end of each grant and
// of the send buffer, and sseq_no and rseq_no wrap past 65536 messages.
func TestStreamsCarryBytesIntact(t *testing.T) {
	a, b := join(smallConfig(0xa0000000), smallConfig(0xb0000000))
	flows := []*flow{{w: a, r: b}, {w: b, r: a}}
	writes, reads := []int{1, 7, 300}, []int{5, 1000, 13}
	rng := rand.NewChaCha8([32]byte{})
	const messages = 1<<16 + 2*rsproto.QPMinSize

	for done := false; !done; {
		moved := false
		for _, f := range flows {
			for f.w.sent < messages {
				v := f.w.s.Reserve()
				if v == nil {
					if uint16(f.w.sent) == f.w.limit {
						f.stalls++
					}
					break
				}
				v = v[:min(len(v), writes[f.w.sent%len(writes)])]
				rng.Read(v)
				f.sent = append(f.sent, v...)
				if err := f.w.s.Commit(len(v)); err != nil {
					t.Fatalf("Commit(%d) = %v", len(v), err)
				}
				f.w.sent++
				moved = true
			}
		}
		moved = pump(a, b) || moved
		done = true
		for _, f := range flows {
			for v := f.r.s.Peek(); len(v) > 0; v = f.r.s.Peek() {
				if &v[0] == &f.r.c.ReceiveBuffer.Buf[0] {
					f.wraps++
				}
				v = v[:min(len(v), reads[len(f.got)%len(reads)])]
				f.got = append(f.got, v...)
				f.r.s.Consume(len(v))
				moved = true
			}
			done = done && f.w.sent == messages && len(f.got) == len(f.sent)
		}
		moved = pump(a, b) || moved
		if !done && !moved {
			t.Fatalf("stalled with %d and %d messages sent, %d of %d and %d of %d bytes read", a.sent, b.sent, len(flows[0].got), len(flows[0].sent), len(flows[1].got), len(flows[1].sent))
		}
	}
	for i, f := range flows {
		if !bytes.Equal(f.got, f.sent) {
			t.Errorf("flow %d read %d bytes that differ from the %d written", i, len(f.got), len(f.sent))
		}
		if f.stalls == 0 || f.wraps < 3 {
			t.Errorf("flow %d ran out of credits %d times and lent the start of the receive buffer %d times, want both several", i, f.stalls, f.wraps)
		}
	}
}

// rshutdown(SHUT_WR) ends the peer's reading behind the data, with
// RS_CTRL_SHUTDOWN, while the peer goes on writing; the second side to shut
// down writing sends RS_CTRL_DISCONNECT, or both send RS_CTRL_SHUTDOWN when
// they do so at once, and either way both end in order.
func TestHalfClosesBothWays(t *testing.T) {
	tests := []struct {
		name  string
		close func(t *testing.T, a, b *side)
	}{
		{"one after the other", func(t *testing.T, a, b *side) {
			send(t, a, "from a")
			a.s.CloseWrite()
			pump(a, b)
			if got := recv(b); got != "from a" || !b.s.EOF() {
				t.Fatalf("b read %q and EOF() = %v after a shut down writing, want %q and true", got, b.s.EOF(), "from a")
			}
			if want := rsproto.StateConnected | rsproto.StateWritable; b.s.State() != want {
				t.Fatalf("b is in state %#x after a shut down writing, want %#x", b.s.State(), want)
			}
			send(t, b, "from b")
			b.s.CloseWrite()
			pump(a, b)
			if got := recv(a); got != "from b" || !a.s.EOF() {
				t.Fatalf("a read %q and EOF() = %v after b shut down writing, want %q and true", got, a.s.EOF(), "from b")
			}
		}},
		{"at once", func(t *testing.T, a, b *side) {
			send(t, a, "from a")
			send(t, b, "from b")
			a.s.CloseWrite()
			b.s.CloseWrite()
			pump(a, b)
			if got, got2 := recv(a), recv(b); got != "from b" || got2 != "from a" || !a.s.EOF() || !b.s.EOF() {
				t.Fatalf("a and b read %q and %q, EOF() %v and %v, want %q, %q and true", got, got2, a.s.EOF(), b.s.EOF(), "from b", "from a")
			}
		}},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			a, b := join(smallConfig(0xa0000000), smallConfig(0xb0000000))
			tt.close(t, a, b)
			for _, x := range []*side{a, b} {
				if x.s.State() != rsproto.StateDisconnected || x.s.Err() != io.EOF || !x.s.SendsDone() {
					t.Errorf("a side ended in state %#x with Err() = %v and SendsDone() = %v, want %#x, io.EOF and true", x.s.State(), x.s.Err(), x.s.SendsDone(), rsproto.StateDisconnected)
				}
			}
		})
	}
}

// rclose, rshutdown(SHUT_RDWR), sends RS_CTRL_DISCONNECT behind the data: the
// peer reads the data, then the end, and writes no more, and both end in
// order.
func TestDisconnectEndsBothInOrder(t *testing.T) {
	a, b := join(smallConfig(0xa0000000), smallConfig(0xb0000000))
	send(t, a, "last words")
	a.s.Disconnect()
	pump(a, b)

	if got := recv(b); got != "last words" || !b.s.EOF() || b.s.Reserve() != nil {
		t.Errorf("after a's disconnect b read %q, EOF() = %v, and Reserve() lent %d bytes, want %q, true and none", got, b.s.EOF(), len(b.s.Reserve()), "last words")
	}
	for _, x := range []*side{a, b} {
		if x.s.State() != rsproto.StateDisconnected || x.s.Err() != io.EOF {
			t.Errorf("a side ended in state %#x with Err() = %v, want %#x and io.EOF", x.s.State(), x.s.Err(), rsproto.StateDisconnected)
		}
	}
}

// rshutdown and rclose wait for a control slot for their control message and
// drop it once the peer's RS_CTRL_SHUTDOWN, crossing it, makes the rsocket
// rs_disconnected, which leaves the peer that shut down writing to learn the
// end from the loss of the link; the stream sends it at the next free slot,
// and both sides end in order.
func TestHeldEndCrossingAShutdownEndsBothInOrder(t *testing.T) {
	tests := []struct {
		name string
		end  func(s *rsproto.Stream) // what a does as b shuts down writing
	}{
		{"half-closes", (*rsproto.Stream).CloseWrite},
		{"a disconnect and a half-close", (*rsproto.Stream).Disconnect},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			a, b := join(smallConfig(0xa0000000), smallConfig(0xb0000000))
			fillControlSlots(t, a, b)
			tt.end(a.s)
			b.s.CloseWrite()
			a.deliver()
			pump(a, b)
			for _, x := range []*side{a, b} {
				if x.s.State() != rsproto.StateDisconnected || x.s.Err() != io.EOF {
					t.Errorf("a side ended in state %#x with Err() = %v, want %#x and io.EOF", x.s.State(), x.s.Err(), rsproto.StateDisconnected)
				}
			}
		})
	}
}

// RS_CTRL_KEEPALIVE between data changes nothing at the stream it reaches.
// Stock rsockets sends it as an RDMA write without immediate data, which
// never arrives; a peer sending it with immediate data is ignored.
func TestKeepaliveBetweenDataIsIgnored(t *testing.T) {
	a, b := join(smallConfig(0xa0000000), smallConfig(0xb0000000))
	const keepaliveID = 1 << 62
	send(t, a, "before ")
	if err := a.l.PostMsg(keepaliveID, rsproto.MakeMsg(rsproto.OpCtrl, rsproto.CtrlKeepalive)); err != nil {
		t.Fatalf("PostMsg of the keepalive = %v", err)
	}
	send(t, a, "after")
	b.deliver()
	for id, err, ok := a.l.Completion(); ok; id, err, ok = a.l.Completion() {
		if id != keepaliveID {
			a.s.Sent(id, err)
		}
	}
	pump(a, b)

	if got, want := recv(b), "before after"; got != want || b.arrived[rsproto.OpCtrl] != 1 {
		t.Errorf("b read %q with %d control messages arrived, want %q and the keepalive", got, b.arrived[rsproto.OpCtrl], want)
	}
	if want := rsproto.StateConnected | rsproto.StateReadable | rsproto.StateWritable; a.s.State() != want || b.s.State() != want {
		t.Errorf("after the keepalive a and b are in states %#x and %#x, want %#x", a.s.State(), b.s.State(), want)
	}
}

// fillControlSlots has b write to a and a read what arrives, taking none of
// its completions, round after round until the credits a earns no longer
// reach b.
func fillControlSlots(t *testing.T, a, b *side) {
	t.Helper()
	for range 8 {
		for b.s.CanSend() {
			send(t, b, "x")
		}
		for a.deliver() || b.deliver() || b.complete() {
		}
		recv(a)
		for a.deliver() || b.deliver() || b.complete() {
		}
	}
}

// rs_give_credits waits for a control slot, and only completions give slots
// back: while a reader takes none, at most RS_QP_CTRL_SIZE of its credits
// reach the writer, which runs out and waits though the reader has earned
// more; once the reader takes its completions the credits held go out.
func TestAtMostFourControlMessagesInFlight(t *testing.T) {
	a, b := join(smallConfig(0xa0000000), smallConfig(0xb0000000))
	before := b.arrived[rsproto.OpSGL]
	fillControlSlots(t, a, b)
	if got := b.arrived[rsproto.OpSGL] - before; got != rsproto.QPCtrlSize || b.s.CanSend() {
		t.Fatalf("with a's completions held, %d credit messages reached b and CanSend() = %v, want %d and false", got, b.s.CanSend(), rsproto.QPCtrlSize)
	}

	pump(a, b)
	if got := b.arrived[rsproto.OpSGL] - before; got != rsproto.QPCtrlSize+1 || !b.s.CanSend() {
		t.Errorf("once a took its completions, %d credit messages had reached b and CanSend() = %v, want %d and true", got, b.s.CanSend(), rsproto.QPCtrlSize+1)
	}
}

// A control message takes none of the writer's credits, so it can find the
// peer's receives all used up by the data the credits allowed: rsockets
// counts on the RNR retry of the link, and the message arrives once the peer
// has taken the data and posted its receives again.
func TestControlMessageWaitsForAReceive(t *testing.T) {
	a, b := join(smallConfig(0xa0000000), smallConfig(0xb0000000))
	for range rsproto.QPMinSize {
		send(t, a, "x")
	}
	if a.s.CanSend() {
		t.Fatalf("a can send with the %d credits of b used up", rsproto.QPMinSize)
	}
	var held []rsproto.Msg
	for msg, ok := b.l.Deliver(); ok; msg, ok = b.l.Deliver() {
		held = append(held, msg)
	}
	a.s.CloseWrite()
	if msg, ok := b.l.Deliver(); ok {
		t.Fatalf("Deliver() with the %d receives of b used up = %#x, want nothing", len(held), msg)
	}

	for _, msg := range held {
		b.s.Received(msg)
	}
	pump(a, b)
	if got, want := recv(b), strings.Repeat("x", rsproto.QPMinSize); got != want || !b.s.EOF() {
		t.Errorf("once b posted its receives again it read %q and EOF() = %v, want %q and true", got, b.s.EOF(), want)
	}
}

// A write that fails breaks the stream that made it, which then writes no
// more, so that no message goes out behind the one lost: the stream is in the
// error state, with the failure for its cause, not the io.EOF of an orderly
// end.
func TestFailedWriteIsNoOrderlyEnd(t *testing.T) {
	failed := errors.New("injected failure")
	tests := []struct {
		name  string
		write func(t *testing.T, a, b *side)
	}{
		{"post refused", func(t *testing.T, a, b *side) {
			a.l.FailPost(failed)
			a.s.Reserve()
			if err := a.s.Commit(1); !errors.Is(err, failed) {
				t.Fatalf("Commit(1) with the post refused = %v, want %v", err, failed)
			}
		}},
		{"completion failed", func(t *testing.T, a, b *side) {
			a.l.FailSend(failed)
			send(t, a, "lost")
			pump(a, b)
			if got := recv(b); got != "" {
				t.Fatalf("b read %q of the write that failed", got)
			}
		}},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			a, b := join(smallConfig(0xa0000000), smallConfig(0xb0000000))
			tt.write(t, a, b)
			if err := a.s.Err(); a.s.State() != rsproto.StateError || !errors.Is(err, failed) || errors.Is(err, io.EOF) || a.s.Reserve() != nil {
				t.Errorf("a failed write left the state %#x, Err() = %v and Reserve() lending %d bytes, want %#x, an error wrapping %q and none", a.s.State(), err, len(a.s.Reserve()), rsproto.StateError, failed)
			}
		})
	}
}

// down hands each stream the loss of the link once its end reports it.
func down(xs ...*side) {
	for _, x := range xs {
		if err := x.l.Down(); err != nil {
			x.s.LinkDown(err)
		}
	}
}

// The loss of the link ends both streams with it for their cause, directly
// or through the failure of a send in flight it flushed, not with the io.EOF
// of an orderly end; once a stream has ended in order, the loss changes
// nothing, though it come before the stream took its last completion.
func TestLinkLossIsNoOrderlyEnd(t *testing.T) {
	lost := errors.New("RDMA_CM_EVENT_DISCONNECTED")
	t.Run("while connected", func(t *testing.T) {
		a, b := join(smallConfig(0xa0000000), smallConfig(0xb0000000))
		send(t, a, "in flight")
		a.l.Break(lost)
		down(a, b)
		pump(a, b)
		down(a, b)
		for _, x := range []*side{a, b} {
			if err := x.s.Err(); !errors.Is(err, lost) || errors.Is(err, io.EOF) || x.s.State()&rsproto.StateConnected != 0 {
				t.Errorf("the loss left a side in state %#x with Err() = %v, want it ended with an error wrapping %q", x.s.State(), err, lost)
			}
		}
	})
	t.Run("after an orderly end", func(t *testing.T) {
		a, b := join(smallConfig(0xa0000000), smallConfig(0xb0000000))
		a.s.Disconnect()
		b.deliver()
		a.l.Break(lost)
		down(a, b)
		pump(a, b)
		down(a, b)
		for _, x := range []*side{a, b} {
			if x.s.State() != rsproto.StateDisconnected || x.s.Err() != io.EOF {
				t.Errorf("the loss after the disconnect left a side in state %#x with Err() = %v, want %#x and io.EOF", x.s.State(), x.s.Err(), rsproto.StateDisconnected)
			}
		}
	})
}

// The ends serve two sides driven from goroutines of their own, as an engine
// drives them: on each side a goroutine takes the events of its end and
// another moves bytes through the views of its stream, both under the side's
// lock but for the copy into or out of the view. The bytes cross intact, and
// the race detector finds no access to the memory of a side that the link
// leaves unordered.
func TestEndsServeTheirOwnGoroutines(t *testing.T) {
	a, b := join(smallConfig(0xa0000000), smallConfig(0xb0000000))
	sent := make([]byte, 1<<20)
	rand.NewChaCha8([32]byte{1}).Read(sent)
	var mu [2]sync.Mutex
	stop := make(chan struct{})
	var wg sync.WaitGroup
	for i, x := range []*side{a, b} {
		wg.Go(func() {
			for {
				select {
				case <-stop:
					return
				default:
				}
				mu[i].Lock()
				for x.deliver() || x.complete() {
				}
				mu[i].Unlock()
				runtime.Gosched()
			}
		})
	}
	wg.Go(func() {
		defer func() {
			mu[0].Lock()
			a.s.CloseWrite()
			mu[0].Unlock()
		}()
		for n := 0; n < len(sent); {
			mu[0].Lock()
			v := a.s.Reserve()
			mu[0].Unlock()
			if v == nil {
				runtime.Gosched()
				continue
			}
			m := copy(v, sent[n:])
			mu[0].Lock()
			err := a.s.Commit(m)
			mu[0].Unlock()
			if err != nil {
				t.Errorf("Commit(%d) = %v", m, err)
				return
			}
			n += m
		}
	})

	var got []byte
	for {
		mu[1].Lock()
		v, eof := b.s.Peek(), b.s.EOF()
		mu[1].Unlock()
		if eof {
			break
		}
		if len(v) == 0 {
			runtime.Gosched()
			continue
		}
		got = append(got, v...)
		mu[1].Lock()
		b.s.Consume(len(v))
		mu[1].Unlock()
	}
	close(stop)
	wg.Wait()
	if !bytes.Equal(got, sent) {
		t.Errorf("b read %d bytes that differ from the %d a wrote", len(got), len(sent))
	}
}
