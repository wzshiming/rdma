package rdma

import (
	"errors"
	"fmt"
	"time"
)

// Config carries the rsockets options of a connection. A zero option takes
// the rsockets default, the compile-time one each field gives. A Dialer's
// Config applies to the connection it dials, a ListenConfig's to the listener
// and, by inheritance, to every connection it accepts.
type Config struct {
	// SendBufferSize is the size in bytes of the registered send buffer
	// (SO_SNDBUF), which rsockets double like the kernel does. Default 128
	// KiB.
	SendBufferSize int
	// ReceiveBufferSize is the size in bytes of the registered receive
	// buffer (SO_RCVBUF), doubled likewise; the peer may have this much in
	// flight before it waits. Default 128 KiB.
	ReceiveBufferSize int
	// SendQueueSize is the depth of the send work queue (RDMA_SQSIZE).
	// Default 384.
	SendQueueSize int
	// ReceiveQueueSize is the depth of the receive work queue
	// (RDMA_RQSIZE). Default 384.
	ReceiveQueueSize int
	// MaxInline is the largest send in bytes posted inline (RDMA_INLINE).
	// Default 64.
	MaxInline int
	// PollTime is how long a Read or Write that has to wait polls for
	// completions before its goroutine sleeps. Zero does not poll, unlike
	// stock rsockets, which poll for 10 µs by default (polling_time).
	PollTime time.Duration
}

// errConfig is the cause of the *net.OpError reported for a Config that no
// socket could take.
var errConfig = errors.New("invalid Config")

// validate rejects negative fields before a socket is created.
func (c Config) validate() error {
	for _, f := range []struct {
		name  string
		value int
	}{
		{"SendBufferSize", c.SendBufferSize},
		{"ReceiveBufferSize", c.ReceiveBufferSize},
		{"SendQueueSize", c.SendQueueSize},
		{"ReceiveQueueSize", c.ReceiveQueueSize},
		{"MaxInline", c.MaxInline},
	} {
		if f.value < 0 {
			return fmt.Errorf("%w: %s %d is negative", errConfig, f.name, f.value)
		}
	}
	if c.PollTime < 0 {
		return fmt.Errorf("%w: PollTime %v is negative", errConfig, c.PollTime)
	}
	return nil
}
