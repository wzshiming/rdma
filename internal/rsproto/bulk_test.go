package rsproto_test

import (
	"bytes"
	"fmt"
	"math/rand/v2"
	"strings"
	"testing"

	"github.com/wzshiming/rdma/internal/rsproto"
)

// bufferConfig describes memory of the stock queues and of buffers of the
// sizes given, from base on.
func bufferConfig(base uint64, send, receive int) rsproto.Config {
	return rsproto.Config{
		SendBuffer:       rsproto.Region{Buf: make([]byte, send), Addr: base, Key: 1},
		ReceiveBuffer:    rsproto.Region{Buf: make([]byte, receive), Addr: base + 1<<28, Key: 2},
		GrantTable:       rsproto.Region{Buf: make([]byte, rsproto.SGLSize*rsproto.SGESize), Addr: base + 2<<28, Key: 3},
		ControlBuffer:    rsproto.Region{Buf: make([]byte, rsproto.QPCtrlSize*rsproto.SGESize), Addr: base + 3<<28, Key: 4},
		SendQueueSize:    rsproto.DefaultSendQueueSize,
		ReceiveQueueSize: rsproto.DefaultReceiveQueueSize,
	}
}

// bulkWrite moves total bytes from a to b in writes of write bytes, each
// committed through the views a lends, while b reads all that has arrived,
// in turn with the messages and completions carried between, and returns the
// message words that reached b and those that reached a, in order.
func bulkWrite(t *testing.T, a, b *side, total, write int) (toB, toA []rsproto.Msg) {
	t.Helper()
	carry := func() bool {
		moved := false
		for carried := true; carried; moved = moved || carried {
			carried = false
			for _, x := range []struct {
				end *side
				log *[]rsproto.Msg
			}{{b, &toB}, {a, &toA}} {
				for msg, ok := x.end.l.Deliver(); ok; msg, ok = x.end.l.Deliver() {
					*x.log = append(*x.log, msg)
					x.end.s.Received(msg)
					carried = true
				}
				carried = x.end.complete() || carried
			}
		}
		return moved
	}
	sent := make([]byte, total)
	rand.NewChaCha8([32]byte{}).Read(sent)
	var got []byte
	for p, left := sent, 0; len(got) < total; {
		moved := false
		for len(p) > 0 {
			if left == 0 {
				left = min(write, len(p))
			}
			v := a.s.Reserve()
			if v == nil {
				break
			}
			n := copy(v, p[:left])
			if err := a.s.Commit(n); err != nil {
				t.Fatalf("Commit(%d) = %v", n, err)
			}
			p, left = p[n:], left-n
			moved = true
		}
		moved = carry() || moved
		for v := b.s.Peek(); len(v) > 0; v = b.s.Peek() {
			got = append(got, v...)
			b.s.Consume(len(v))
			moved = true
		}
		if !moved {
			t.Fatalf("stalled with %d of %d bytes written and %d read", total-len(p), total, len(got))
		}
	}
	carry()
	if !bytes.Equal(got, sent) {
		t.Errorf("b read %d bytes that differ from the %d a wrote", len(got), total)
	}
	return toB, toA
}

// opNames are the C names of the ops of the message words.
var opNames = map[rsproto.Op]string{rsproto.OpData: "RS_OP_DATA", rsproto.OpSGL: "RS_OP_SGL", rsproto.OpCtrl: "RS_OP_CTRL"}

// words lists message words by op and data, a run of equal words once with
// its length.
func words(ms []rsproto.Msg) string {
	var b strings.Builder
	for i := 0; i < len(ms); {
		j := i + 1
		for j < len(ms) && ms[j] == ms[i] {
			j++
		}
		if b.Len() > 0 {
			b.WriteString(", ")
		}
		fmt.Fprintf(&b, "%s %d", opNames[ms[i].Op()], ms[i].Data())
		if j-i > 1 {
			fmt.Fprintf(&b, " x%d", j-i)
		}
		i = j
	}
	return b.String()
}

// The messages of a bulk transfer between streams of the stock buffers, 128
// KiB each way, which grant 64 KiB at a time, half a receive buffer: the
// writer's data and the reader's grants, message for message.
func TestBulkTransferMessagesWithStockBuffers(t *testing.T) {
	for _, tt := range []struct {
		name         string
		total, write int
		toB, toA     string
	}{
		{
			"a write of 1 MiB", 1 << 20, 1 << 20,
			"RS_OP_DATA 65536 x16",
			"RS_OP_SGL 386 x2, RS_OP_SGL 388 x2, RS_OP_SGL 390 x2, RS_OP_SGL 392 x2, RS_OP_SGL 394 x2, RS_OP_SGL 396 x2, RS_OP_SGL 398 x2, RS_OP_SGL 400 x2",
		},
		{
			"writes of 100000 bytes", 300000, 100000,
			"RS_OP_DATA 65536, RS_OP_DATA 34464, RS_OP_DATA 31072, RS_OP_DATA 65536, RS_OP_DATA 3392, RS_OP_DATA 62144, RS_OP_DATA 37856",
			"RS_OP_SGL 387 x2, RS_OP_SGL 390 x2",
		},
	} {
		t.Run(tt.name, func(t *testing.T) {
			send, receive := rsproto.DefaultSendBufferSize, rsproto.DefaultReceiveBufferSize
			a, b := join(bufferConfig(0xa0000000, send, receive), bufferConfig(0xb0000000, send, receive))
			toB, toA := bulkWrite(t, a, b, tt.total, tt.write)
			if got := words(toB); got != tt.toB {
				t.Errorf("a wrote %d bytes as %s, want %s", tt.total, got, tt.toB)
			}
			if got := words(toA); got != tt.toA {
				t.Errorf("b answered with %s, want %s", got, tt.toA)
			}
		})
	}
}

// Between streams of 1 MiB buffers each way, which grant 512 KiB at a time,
// a write of 1 MiB goes out in a message a grant: two, where the 64 KiB of
// RS_MAX_TRANSFER make sixteen.
func TestLargeBuffersSendAMessageAGrant(t *testing.T) {
	a, b := join(bufferConfig(0xa0000000, 1<<20, 1<<20), bufferConfig(0xb0000000, 1<<20, 1<<20))
	toB, _ := bulkWrite(t, a, b, 1<<20, 1<<20)
	if got, want := words(toB), "RS_OP_DATA 524288 x2"; got != want {
		t.Errorf("a wrote 1 MiB between 1 MiB buffers as %s, want %s", got, want)
	}
}
