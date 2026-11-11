package rdma

import (
	"errors"
	"fmt"
	"math"

	"github.com/wzshiming/rdma/internal/rsproto"
	"github.com/wzshiming/rdma/verbs"
)

// nativeEngine is the engine that speaks the rsockets protocol in Go, each
// connection a native stream on an RC queue pair of verbs that cm connects.
type nativeEngine struct {
	// transportType reports the transport of a device, nil meaning the
	// device's own; tests make a device look like iWARP.
	transportType func(*verbs.Context) verbs.TransportType
}

// errIWARP refuses a device of the iWARP transport, whose mode of rsockets
// the native engine leaves out.
var errIWARP = fmt.Errorf("rdma: the native engine does not run on iWARP devices, the librdmacm engine does: %w", errors.ErrUnsupported)

// checkDevice reports why the engine cannot run on dev, nil when it can: on
// every transport but iWARP, the one rs_create_ep sets up differently.
func (e nativeEngine) checkDevice(dev *verbs.Context) error {
	transport := e.transportType
	if transport == nil {
		transport = (*verbs.Context).TransportType
	}
	if transport(dev) == verbs.TransportIWARP {
		return errIWARP
	}
	return nil
}

// connSizes are the sizes of the buffers and queues of a native connection.
type connSizes struct {
	sendBuffer, receiveBuffer int // bytes
	sendQueue, receiveQueue   int // work requests
	inline                    int // bytes the queue pair is asked to take inline
}

// sizesFor sizes a connection on a device whose queues hold at most maxQueue
// work requests as rsockets sizes an rsocket that the librdmacm engine set
// c's non-zero fields on: rsetsockopt doubles the buffer sizes, giving the
// send buffer 4096 bytes should it fall short of RS_SNDLOWAT, and caps the
// queue depths and the inline size at 0xFFFE, and rs_set_qp_size keeps the
// depths within 16 and the device's limit. Zero fields take the compile-time
// defaults of rsockets, never those of /etc/rdma/rsocket. A buffer beyond the
// 32-bit sizes of rsockets is refused.
func sizesFor(c Config, maxQueue int) (connSizes, error) {
	s := connSizes{
		sendBuffer:    rsproto.DefaultSendBufferSize,
		receiveBuffer: rsproto.DefaultReceiveBufferSize,
		sendQueue:     rsproto.DefaultSendQueueSize,
		receiveQueue:  rsproto.DefaultReceiveQueueSize,
		inline:        rsproto.DefaultMaxInline,
	}
	for _, f := range []struct {
		name  string
		value int
	}{{"SendBufferSize", c.SendBufferSize}, {"ReceiveBufferSize", c.ReceiveBufferSize}} {
		if f.value > math.MaxUint32/2 {
			return connSizes{}, fmt.Errorf("%w: %s %d exceeds the 32 bits of rsockets once doubled", errConfig, f.name, f.value)
		}
	}
	if c.SendBufferSize != 0 {
		if s.sendBuffer = 2 * c.SendBufferSize; s.sendBuffer < rsproto.SndLowat {
			s.sendBuffer = 2 * rsproto.SndLowat
		}
	}
	if c.ReceiveBufferSize != 0 {
		s.receiveBuffer = 2 * c.ReceiveBufferSize
	}
	if c.SendQueueSize != 0 {
		s.sendQueue = min(c.SendQueueSize, rsproto.QPMaxSize)
	}
	if c.ReceiveQueueSize != 0 {
		s.receiveQueue = min(c.ReceiveQueueSize, rsproto.QPMaxSize)
	}
	if c.MaxInline != 0 {
		s.inline = min(c.MaxInline, rsproto.QPMaxSize)
	}
	limit := min(maxQueue, rsproto.QPMaxSize)
	s.sendQueue = queueDepth(s.sendQueue, limit)
	s.receiveQueue = queueDepth(s.receiveQueue, limit)
	return s, nil
}

// queueDepth keeps the depth n within 16 and limit as rs_set_qp_size does.
func queueDepth(n, limit int) int {
	switch {
	case n > limit:
		return limit
	case n < rsproto.QPMinSize:
		return rsproto.QPMinSize
	}
	return n
}
