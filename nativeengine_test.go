package rdma

import (
	"errors"
	"math"
	"testing"
)

// A native connection is sized as rsockets sizes an rsocket that the
// librdmacm engine set the same Config on: buffer sizes doubled, a send
// buffer of 4096 bytes when that falls short of RS_SNDLOWAT, queue depths and
// the inline size at most 0xFFFE, queue depths at least 16 and within the
// device's limit, and the compile-time defaults of rsockets for zero.
func TestNativeSizesFollowRsockets(t *testing.T) {
	const roomy = 1 << 20 // a device limit beyond every depth asked for
	for _, tt := range []struct {
		name     string
		c        Config
		maxQueue int
		want     connSizes
	}{
		{"defaults", Config{}, roomy, connSizes{1 << 17, 1 << 17, 384, 384, 64}},
		{"buffers doubled", Config{SendBufferSize: 1500, ReceiveBufferSize: 3}, roomy, connSizes{3000, 6, 384, 384, 64}},
		{"send buffer short of RS_SNDLOWAT", Config{SendBufferSize: 1023}, roomy, connSizes{4096, 1 << 17, 384, 384, 64}},
		{"send buffer at RS_SNDLOWAT", Config{SendBufferSize: 1024}, roomy, connSizes{2048, 1 << 17, 384, 384, 64}},
		{"queues below 16", Config{SendQueueSize: 1, ReceiveQueueSize: 15}, roomy, connSizes{1 << 17, 1 << 17, 16, 16, 64}},
		{"queues beyond the device", Config{SendQueueSize: 2000, ReceiveQueueSize: 1001}, 1000, connSizes{1 << 17, 1 << 17, 1000, 1000, 64}},
		{"defaults beyond the device", Config{}, 100, connSizes{1 << 17, 1 << 17, 100, 100, 64}},
		{"queues beyond rsockets", Config{SendQueueSize: 1 << 20, ReceiveQueueSize: 0xFFFF}, roomy, connSizes{1 << 17, 1 << 17, 0xFFFE, 0xFFFE, 64}},
		{"inline", Config{MaxInline: 1}, roomy, connSizes{1 << 17, 1 << 17, 384, 384, 1}},
		{"inline beyond rsockets", Config{MaxInline: 1 << 20}, roomy, connSizes{1 << 17, 1 << 17, 384, 384, 0xFFFE}},
	} {
		t.Run(tt.name, func(t *testing.T) {
			got, err := sizesFor(tt.c, tt.maxQueue)
			if err != nil || got != tt.want {
				t.Errorf("sizesFor(%+v, %d) = %+v, %v; want %+v, nil", tt.c, tt.maxQueue, got, err, tt.want)
			}
		})
	}
}

// A buffer whose doubled size the 32 bits of rsockets cannot hold is refused
// as an invalid Config.
func TestNativeSizesRefuseBuffersBeyond32Bits(t *testing.T) {
	if math.MaxInt == math.MaxInt32 {
		t.Skip("an int holds no such size here")
	}
	fits := math.MaxUint32 / 2
	for _, c := range []Config{{SendBufferSize: fits + 1}, {ReceiveBufferSize: fits + 1}} {
		if got, err := sizesFor(c, 1<<20); !errors.Is(err, errConfig) {
			t.Errorf("sizesFor(%+v) = %+v, %v; want an error matching errConfig", c, got, err)
		}
	}
	c := Config{SendBufferSize: fits, ReceiveBufferSize: fits}
	if got, err := sizesFor(c, 1<<20); err != nil || got.sendBuffer != 2*fits || got.receiveBuffer != 2*fits {
		t.Errorf("sizesFor(%+v) = %+v, %v; want buffers of %d bytes", c, got, err, 2*fits)
	}
}
