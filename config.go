package rdma

import "fmt"

// Config tunes the buffers of a connection. The zero value of each field
// selects its default.
type Config struct {
	// MessageSize is the size in bytes of one message on the wire, header
	// included, and so of each registered buffer; it is negotiated down to
	// the smaller of the two peers' values. Default 16 KiB, minimum 256.
	MessageSize int
	// ReceiveDepth is the number of messages the peer may have in flight
	// toward this end, each backed by a posted receive buffer; it is
	// negotiated down to the smaller of the two peers' values. Default 32.
	ReceiveDepth int
	// SendDepth is the number of messages this end may have in flight before
	// Write waits for a send completion. Default 32.
	SendDepth int
	// MaxInline is the largest message the queue pair is asked to send
	// inline. 0 accepts whatever the device grants; the value is never
	// required.
	MaxInline int
}

// Config defaults.
const (
	DefaultMessageSize  = 16 << 10
	DefaultReceiveDepth = 32
	DefaultSendDepth    = 32
)

// minMessageSize is the smallest MessageSize: the header plus a little room.
const minMessageSize = 256

// withDefaults returns c with every zero field replaced by its default.
func (c Config) withDefaults() Config {
	if c.MessageSize == 0 {
		c.MessageSize = DefaultMessageSize
	}
	if c.ReceiveDepth == 0 {
		c.ReceiveDepth = DefaultReceiveDepth
	}
	if c.SendDepth == 0 {
		c.SendDepth = DefaultSendDepth
	}
	return c
}

// validate rejects values no device could serve; device limits are checked
// when the connection is set up.
func (c Config) validate() error {
	switch {
	case c.MessageSize < minMessageSize:
		return fmt.Errorf("rdma: MessageSize %d is below the minimum of %d", c.MessageSize, minMessageSize)
	case c.MessageSize > maxMessageSize:
		return fmt.Errorf("rdma: MessageSize %d exceeds the maximum of %d", c.MessageSize, maxMessageSize)
	case c.ReceiveDepth < 1 || c.ReceiveDepth > maxDepth:
		return fmt.Errorf("rdma: ReceiveDepth %d is not between 1 and %d", c.ReceiveDepth, maxDepth)
	case c.SendDepth < 1 || c.SendDepth > maxDepth:
		return fmt.Errorf("rdma: SendDepth %d is not between 1 and %d", c.SendDepth, maxDepth)
	case c.MaxInline < 0:
		return fmt.Errorf("rdma: MaxInline %d is negative", c.MaxInline)
	}
	return nil
}

// Bounds that keep the handshake fields and buffer sizes in range.
const (
	maxMessageSize = 1 << 30
	maxDepth       = 1 << 16
)
