package rsproto_test

import (
	"bytes"
	"errors"
	"testing"

	"github.com/wzshiming/rdma/internal/rsproto"
)

// An RDMA write with immediate data gathers its local segments into the
// memory the peer registered under the key it names, at the address it
// names, and its message word arrives with it.
func TestMemLinkWriteLandsAtAddressAndKey(t *testing.T) {
	a, b := rsproto.NewMemLink()
	src := rsproto.Region{Buf: []byte("hel--lo"), Addr: 0x1000, Key: 1}
	other := rsproto.Region{Buf: make([]byte, 16), Addr: 0x2000, Key: 2}
	dst := rsproto.Region{Buf: make([]byte, 16), Addr: 0x2000, Key: 3}
	a.Register(src)
	b.Register(other)
	b.Register(dst)
	if err := b.PostRecv(1 << 63); err != nil {
		t.Fatalf("PostRecv = %v", err)
	}

	sgl := []rsproto.Segment{{Addr: 0x1000, Length: 3, Key: 1}, {Addr: 0x1005, Length: 2, Key: 1}}
	msg := rsproto.MakeMsg(rsproto.OpData, 5)
	if err := a.PostWriteMsg(7, sgl, msg, 0x2004, 3); err != nil {
		t.Fatalf("PostWriteMsg = %v", err)
	}
	if got, ok := b.Deliver(); !ok || got != msg {
		t.Fatalf("Deliver() = %#x, %v, want the message word %#x", got, ok, msg)
	}
	if want := []byte("\x00\x00\x00\x00hello\x00\x00\x00\x00\x00\x00\x00"); !bytes.Equal(dst.Buf, want) || !bytes.Equal(other.Buf, make([]byte, 16)) {
		t.Errorf("the write of %q to 0x2004 under key 3 left key 3 holding %q and key 2 %q, want %q and zeros", "hello", dst.Buf, other.Buf, want)
	}
}

// A write to memory the peer has not registered under the key it names, or
// from memory this end has not, lands nothing and completes with an error,
// as IBV_WC_REM_ACCESS_ERR and IBV_WC_LOC_PROT_ERR do, which leaves the link
// carrying nothing more.
func TestMemLinkWriteOutsideRegisteredMemoryFails(t *testing.T) {
	tests := []struct {
		name string
		seg  rsproto.Segment
		addr uint64
		key  uint32
	}{
		{"remote key unknown", rsproto.Segment{Addr: 0x1000, Length: 4, Key: 1}, 0x2000, 9},
		{"remote address before the memory", rsproto.Segment{Addr: 0x1000, Length: 4, Key: 1}, 0x1ffe, 3},
		{"remote bytes past its end", rsproto.Segment{Addr: 0x1000, Length: 4, Key: 1}, 0x200d, 3},
		{"local key unknown", rsproto.Segment{Addr: 0x1000, Length: 4, Key: 9}, 0x2000, 3},
		{"local bytes past its end", rsproto.Segment{Addr: 0x1006, Length: 4, Key: 1}, 0x2000, 3},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			a, b := rsproto.NewMemLink()
			dst := rsproto.Region{Buf: make([]byte, 16), Addr: 0x2000, Key: 3}
			a.Register(rsproto.Region{Buf: []byte("abcdefgh"), Addr: 0x1000, Key: 1})
			b.Register(dst)
			msg := rsproto.MakeMsg(rsproto.OpData, 4)
			if b.PostRecv(1<<63) != nil || a.PostWriteMsg(1, []rsproto.Segment{tt.seg}, msg, tt.addr, tt.key) != nil {
				t.Fatal("the posts failed")
			}

			if got, ok := b.Deliver(); ok || !bytes.Equal(dst.Buf, make([]byte, 16)) {
				t.Fatalf("Deliver() = %#x, %v, leaving %q, want nothing delivered and nothing landed", got, ok, dst.Buf)
			}
			if id, err, ok := a.Completion(); !ok || id != 1 || err == nil {
				t.Fatalf("Completion() = %d, %v, %v, want 1 and an error", id, err, ok)
			}
			if a.PostMsg(2, msg) != nil {
				t.Fatal("PostMsg failed")
			}
			if id, err, ok := a.Completion(); !ok || id != 2 || err == nil {
				t.Errorf("Completion() of a send after the failed write = %d, %v, %v, want 2 and an error", id, err, ok)
			}
		})
	}
}

// A message word alone, an RDMA write with immediate data of no bytes,
// arrives at the peer, once, and not back at its sender.
func TestMemLinkMessageAloneArrivesAtThePeer(t *testing.T) {
	a, b := rsproto.NewMemLink()
	if err := b.PostRecv(1 << 63); err != nil {
		t.Fatalf("PostRecv = %v", err)
	}
	msg := rsproto.MakeMsg(rsproto.OpCtrl, rsproto.CtrlShutdown)
	if err := a.PostMsg(9, msg); err != nil {
		t.Fatalf("PostMsg = %v", err)
	}

	if got, ok := a.Deliver(); ok {
		t.Fatalf("the sender's own Deliver() = %#x, want nothing", got)
	}
	if got, ok := b.Deliver(); !ok || got != msg {
		t.Fatalf("Deliver() = %#x, %v, want the message word %#x", got, ok, msg)
	}
	if got, ok := b.Deliver(); ok {
		t.Errorf("a second Deliver() = %#x, want nothing", got)
	}
}

// Each send completes once the peer has taken it, with the id it was posted
// with, in the order of the posts, which is the order the messages arrive in.
func TestMemLinkSendsCompleteInOrderOnceTaken(t *testing.T) {
	a, b := rsproto.NewMemLink()
	msgs := []rsproto.Msg{rsproto.MakeMsg(rsproto.OpSGL, 1), rsproto.MakeMsg(rsproto.OpSGL, 2), rsproto.MakeMsg(rsproto.OpSGL, 3)}
	for i, msg := range msgs {
		if err := b.PostRecv(1 << 63); err != nil {
			t.Fatalf("PostRecv = %v", err)
		}
		if err := a.PostMsg(uint64(10+i), msg); err != nil {
			t.Fatalf("PostMsg = %v", err)
		}
	}

	for i, msg := range msgs {
		if id, _, ok := a.Completion(); ok {
			t.Fatalf("send %d completed before the peer took it", id)
		}
		if got, ok := b.Deliver(); !ok || got != msg {
			t.Fatalf("Deliver() %d = %#x, %v, want %#x", i, got, ok, msg)
		}
		if id, err, ok := a.Completion(); !ok || id != uint64(10+i) || err != nil {
			t.Fatalf("Completion() once message %d arrived = %d, %v, %v, want %d, nil, true", i, id, err, ok, 10+i)
		}
	}
	if id, _, ok := a.Completion(); ok {
		t.Errorf("Completion() after all three = %d, want nothing", id)
	}
}

// A message arrives only into a receive the peer has posted: while there is
// none, Deliver leaves it on the link, and the messages behind it, to be
// retried, as the stock rnr_retry_count of 7 retries without end, and it does
// not complete.
func TestMemLinkMessageWaitsForAReceive(t *testing.T) {
	a, b := rsproto.NewMemLink()
	first, second := rsproto.MakeMsg(rsproto.OpSGL, 1), rsproto.MakeMsg(rsproto.OpSGL, 2)
	for i, msg := range []rsproto.Msg{first, second} {
		if err := a.PostMsg(uint64(i), msg); err != nil {
			t.Fatalf("PostMsg = %v", err)
		}
	}

	for range 3 {
		if got, ok := b.Deliver(); ok {
			t.Fatalf("Deliver() with no receive posted = %#x, want nothing", got)
		}
	}
	if id, _, ok := a.Completion(); ok {
		t.Fatalf("send %d completed with no receive posted", id)
	}
	for _, want := range []rsproto.Msg{first, second} {
		if err := b.PostRecv(1 << 63); err != nil {
			t.Fatalf("PostRecv = %v", err)
		}
		if got, ok := b.Deliver(); !ok || got != want {
			t.Fatalf("Deliver() with a receive posted = %#x, %v, want %#x", got, ok, want)
		}
		if got, ok := b.Deliver(); ok {
			t.Fatalf("Deliver() with the receive used up = %#x, want nothing", got)
		}
	}
}

// A post the link is told to refuse fails with the error given and puts
// nothing on the link, whatever its kind; the post after it goes through.
func TestMemLinkRefusesTheInjectedPost(t *testing.T) {
	refused := errors.New("ibv_post_send: ENOMEM")
	msg := rsproto.MakeMsg(rsproto.OpSGL, 1)
	tests := []struct {
		name string
		post func(e *rsproto.MemEnd) error
	}{
		{"PostWriteMsg", func(e *rsproto.MemEnd) error { return e.PostWriteMsg(1, nil, msg, 0, 0) }},
		{"PostMsg", func(e *rsproto.MemEnd) error { return e.PostMsg(1, msg) }},
		{"PostRecv", func(e *rsproto.MemEnd) error { return e.PostRecv(1 << 63) }},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			a, b := rsproto.NewMemLink()
			a.FailPost(refused)
			if err := tt.post(a); !errors.Is(err, refused) {
				t.Fatalf("the post told to fail = %v, want %v", err, refused)
			}
			if err := tt.post(a); err != nil {
				t.Fatalf("the post after it = %v, want nil", err)
			}

			// Two messages each way, two receives at b: one message moves for
			// each post of a that went through, whatever its kind.
			for i := range 2 {
				if b.PostRecv(1<<63) != nil || b.PostMsg(uint64(i), msg) != nil {
					t.Fatal("the posts of b failed")
				}
			}
			moved := 0
			for _, e := range []*rsproto.MemEnd{a, b} {
				for _, ok := e.Deliver(); ok; _, ok = e.Deliver() {
					moved++
				}
			}
			if moved != 1 {
				t.Errorf("%d messages moved after a refused and a good post, want 1", moved)
			}
		})
	}
}

// A send the link is told to fail completes with the error given when the
// peer would take it, instead of arriving, and the link carries nothing
// after it, as a queue pair in the error state: the sends in flight both
// ways, and those posted later, complete with the same error.
func TestMemLinkFailsTheInjectedSend(t *testing.T) {
	failed := errors.New("IBV_WC_RETRY_EXC_ERR")
	msg := rsproto.MakeMsg(rsproto.OpSGL, 1)
	a, b := rsproto.NewMemLink()
	for _, e := range []*rsproto.MemEnd{a, b} {
		for range 2 {
			if err := e.PostRecv(1 << 63); err != nil {
				t.Fatalf("PostRecv = %v", err)
			}
		}
	}
	a.FailSend(failed)
	for _, p := range []struct {
		e  *rsproto.MemEnd
		id uint64
	}{{a, 1}, {a, 2}, {b, 3}} {
		if err := p.e.PostMsg(p.id, msg); err != nil {
			t.Fatalf("PostMsg = %v", err)
		}
	}

	for _, e := range []*rsproto.MemEnd{b, a} {
		if got, ok := e.Deliver(); ok {
			t.Fatalf("Deliver() once the send failed = %#x, want nothing", got)
		}
	}
	if err := a.PostMsg(4, msg); err != nil {
		t.Fatalf("PostMsg after the failure = %v", err)
	}
	for _, want := range []struct {
		e  *rsproto.MemEnd
		id uint64
	}{{a, 1}, {a, 2}, {a, 4}, {b, 3}} {
		if id, err, ok := want.e.Completion(); !ok || id != want.id || !errors.Is(err, failed) {
			t.Errorf("Completion() = %d, %v, %v, want %d, %v, true", id, err, ok, want.id, failed)
		}
	}
}

// The loss of the link completes the sends in flight both ways with the
// error given, carries nothing more, and reaches each end through Down once
// that end has taken the completions before it, for its stream to take
// them first.
func TestMemLinkLossComesAfterTheCompletions(t *testing.T) {
	lost := errors.New("RDMA_CM_EVENT_DISCONNECTED")
	msg := rsproto.MakeMsg(rsproto.OpSGL, 1)
	a, b := rsproto.NewMemLink()
	for i, e := range []*rsproto.MemEnd{a, b} {
		if e.PostRecv(1<<63) != nil || e.PostMsg(uint64(i+1), msg) != nil {
			t.Fatal("the posts failed")
		}
		if err := e.Down(); err != nil {
			t.Fatalf("Down() before the loss = %v, want nil", err)
		}
	}
	a.Break(lost)

	for i, e := range []*rsproto.MemEnd{a, b} {
		if got, ok := e.Deliver(); ok {
			t.Errorf("Deliver() after the loss = %#x, want nothing", got)
		}
		if err := e.Down(); err != nil {
			t.Errorf("Down() with a completion not taken = %v, want nil", err)
		}
		if id, err, ok := e.Completion(); !ok || id != uint64(i+1) || !errors.Is(err, lost) {
			t.Errorf("Completion() after the loss = %d, %v, %v, want %d, %v, true", id, err, ok, i+1, lost)
		}
		if err := e.Down(); !errors.Is(err, lost) {
			t.Errorf("Down() with the completions taken = %v, want %v", err, lost)
		}
	}
}
