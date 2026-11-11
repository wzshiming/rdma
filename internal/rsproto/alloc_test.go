package rsproto_test

import (
	"testing"

	"github.com/wzshiming/rdma/internal/rsproto"
)

// Once the queues of the link have grown to what two streams keep in flight,
// a message allocates nothing on the heap, at the stream or at the link: not
// the commit that sends it, its delivery with the receive posted in place of
// the one it used up, its completion, the read that returns its room, nor the
// grants and credits the reads earn, with their delivery and completion.
func TestSteadyStateMessagesAllocateNothing(t *testing.T) {
	a, b := join(smallConfig(0xa0000000), smallConfig(0xb0000000))
	payload := make([]byte, 100)
	const messages = 1000
	exchange := func() {
		for range messages {
			for _, x := range [...]*side{a, b} {
				v := x.s.Reserve()
				if v == nil {
					t.Fatal("no room to write")
				}
				if err := x.s.Commit(copy(v, payload)); err != nil {
					t.Fatalf("Commit = %v", err)
				}
			}
			pump(a, b)
			for _, x := range [...]*side{a, b} {
				for v := x.s.Peek(); len(v) > 0; v = x.s.Peek() {
					x.s.Consume(len(v))
				}
			}
			pump(a, b)
		}
	}
	credits := a.arrived[rsproto.OpSGL]
	// AllocsPerRun rounds down to whole allocations per run, so one run of
	// many messages counts every allocation.
	if n := testing.AllocsPerRun(1, exchange); n != 0 {
		t.Errorf("%d messages each way allocated %v times, want none", messages, n)
	}
	if got := a.arrived[rsproto.OpSGL] - credits; got < 2*messages/rsproto.QPMinSize {
		t.Errorf("%d grants and credits reached a during %d messages each way, want at least %d", got, 2*messages, 2*messages/rsproto.QPMinSize)
	}
}
