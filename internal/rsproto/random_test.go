package rsproto_test

import (
	"bytes"
	"fmt"
	"io"
	"math/rand/v2"
	"testing"

	"github.com/wzshiming/rdma/internal/rsproto"
)

// randomConfig describes memory and queues of random sizes as small as
// rsockets allows, from base on: a send buffer of at least RS_SNDLOWAT, a
// receive buffer of an even length, as it is granted in halves, queues of
// RS_QP_MIN_SIZE to three times that, and a host of either byte order.
func randomConfig(rng *rand.Rand, base uint64) rsproto.Config {
	c := smallConfig(base)
	c.SendBuffer.Buf = make([]byte, rsproto.SndLowat+rng.IntN(3*rsproto.SndLowat))
	c.ReceiveBuffer.Buf = make([]byte, 2*(256+rng.IntN(4*rsproto.SndLowat)))
	c.SendQueueSize = rsproto.QPMinSize + rng.IntN(2*rsproto.QPMinSize)
	c.ReceiveQueueSize = rsproto.QPMinSize + rng.IntN(2*rsproto.QPMinSize)
	c.BigEndian = rng.IntN(2) == 0
	return c
}

// granule returns a random size from 0 to n, a small one about as likely as
// a large one.
func granule(rng *rand.Rand, n int) int {
	return rng.IntN(min(n, 1<<rng.IntN(14)) + 1)
}

// happen makes one of the events happen, drawn one after the other with
// chances by their weights until one can, and reports whether one could.
func happen(rng *rand.Rand, events []func() bool, weights []int) bool {
	total := 0
	for _, w := range weights {
		total += w
	}
	tried := make([]bool, len(events))
	for total > 0 {
		r, i := rng.IntN(total), 0
		for ; tried[i] || r >= weights[i]; i++ {
			if !tried[i] {
				r -= weights[i]
			}
		}
		if events[i]() {
			return true
		}
		tried[i] = true
		total -= weights[i]
	}
	return false
}

// randomEnd is one of the two sides of
// TestRandomSchedulesCarryBytesIntactToTheEnd: its stream and its end of the
// link, what it writes and what it is to read, the views it holds, and the
// message words its end has delivered and its stream not yet taken, as a
// completion queue holds them. The methods from lendWrite to shutdown are
// the events of the schedule, each reporting whether it could happen.
type randomEnd struct {
	s             *rsproto.Stream
	l             *rsproto.MemEnd
	out, in       []byte
	written, read int // of out, committed; of in, consumed
	wview, rview  []byte
	delivered     []rsproto.Msg
	closed        bool
}

func (x *randomEnd) lendWrite() bool {
	if x.wview != nil || x.written == len(x.out) {
		return false
	}
	x.wview = x.s.Reserve()
	return x.wview != nil
}

func (x *randomEnd) commitWrite(t *testing.T, rng *rand.Rand) bool {
	if x.wview == nil {
		return false
	}
	n := granule(rng, min(len(x.wview), len(x.out)-x.written))
	copy(x.wview, x.out[x.written:x.written+n])
	if err := x.s.Commit(n); err != nil {
		t.Fatalf("Commit(%d) after %d bytes = %v", n, x.written, err)
	}
	x.written += n
	x.wview = nil
	return true
}

func (x *randomEnd) lendRead() bool {
	if x.rview != nil {
		return false
	}
	if v := x.s.Peek(); len(v) > 0 {
		x.rview = v
		return true
	}
	return false
}

func (x *randomEnd) returnRead(t *testing.T, rng *rand.Rand) bool {
	if x.rview == nil {
		return false
	}
	n := granule(rng, len(x.rview))
	if !bytes.Equal(x.rview[:n], x.in[x.read:min(x.read+n, len(x.in))]) {
		t.Fatalf("the %d bytes read at %d of %d differ from those written", n, x.read, len(x.in))
	}
	x.s.Consume(n)
	x.read += n
	x.rview = nil
	return true
}

func (x *randomEnd) carry() bool {
	msg, ok := x.l.Deliver()
	if ok {
		x.delivered = append(x.delivered, msg)
	}
	return ok
}

func (x *randomEnd) take() bool {
	if len(x.delivered) == 0 {
		return false
	}
	x.s.Received(x.delivered[0])
	x.delivered = x.delivered[1:]
	return true
}

func (x *randomEnd) complete(t *testing.T) bool {
	id, err, ok := x.l.Completion()
	if !ok {
		return false
	}
	if err != nil {
		t.Fatalf("send %#x failed: %v", id, err)
	}
	x.s.Sent(id, nil)
	return true
}

func (x *randomEnd) shutdown() bool {
	if x.closed || x.wview != nil || x.written < len(x.out) {
		return false
	}
	x.s.CloseWrite()
	x.closed = true
	return true
}

// ended reports whether the side has written and read everything, the end
// included, and its connection has ended.
func (x *randomEnd) ended() bool {
	return x.closed && x.read == len(x.in) && x.s.EOF() && x.s.State() == rsproto.StateDisconnected
}

func (x *randomEnd) String() string {
	return fmt.Sprintf("state %#x, %d of %d bytes written, %d of %d read, %d words delivered and not taken, shut down %v, Err() = %v",
		x.s.State(), x.written, len(x.out), x.read, len(x.in), len(x.delivered), x.closed, x.s.Err())
}

// Two streams joined by the link carry a byte stream each way intact to its
// end, and never stall, however the writes, reads and returns of their views
// are cut, from nothing to the whole view, and however the events interleave:
// calls come between the lending of a view and its return, and from phase to
// phase each event runs ahead of the others or lags behind, deliveries ahead
// of the stream taking them until the receives run out. Memory and queues are
// of random sizes as small as rsockets allows, the hosts of random byte
// orders. Each side shuts down writing at a random point once it has written
// everything.
func TestRandomSchedulesCarryBytesIntactToTheEnd(t *testing.T) {
	for seed := range 16 {
		t.Run(fmt.Sprint("seed ", seed), func(t *testing.T) {
			src := rand.NewChaCha8([32]byte{byte(seed)})
			rng := rand.New(src)
			la, lb := rsproto.NewMemLink()
			var ends [2]*randomEnd
			for i, l := range []*rsproto.MemEnd{la, lb} {
				x := newSide(l, randomConfig(rng, 0xa0000000+uint64(i)<<28))
				ends[i] = &randomEnd{s: x.s, l: l, out: make([]byte, rng.IntN(1<<17))}
				src.Read(ends[i].out)
			}
			a, b := ends[0], ends[1]
			a.in, b.in = b.out, a.out
			a.s.Connect(b.s.ConnData())
			b.s.Connect(a.s.ConnData())

			var events []func() bool
			for _, x := range ends {
				events = append(events,
					x.lendWrite, func() bool { return x.commitWrite(t, rng) },
					x.lendRead, func() bool { return x.returnRead(t, rng) },
					x.carry, x.take, func() bool { return x.complete(t) },
					x.shutdown)
			}
			weights := make([]int, len(events))
			const phase, maxSteps = 1024, 1 << 20
			for step := 0; !a.ended() || !b.ended(); step++ {
				if step == maxSteps {
					t.Fatalf("no end after %d steps: a in %v; b in %v", step, a, b)
				}
				if step%phase == 0 {
					for i := range weights {
						weights[i] = 1 << rng.IntN(8)
					}
				}
				if !happen(rng, events, weights) {
					t.Fatalf("stalled after %d steps: a in %v; b in %v", step, a, b)
				}
				for _, x := range ends {
					if err := x.s.Err(); err != nil && err != io.EOF {
						t.Fatalf("a side broke after %d steps: %v", step, x)
					}
				}
			}
		})
	}
}
