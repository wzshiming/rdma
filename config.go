package rdma

import (
	"errors"
	"fmt"
)

// Config carries the rsockets options of a connection. The zero value of a
// field leaves the option alone, so the connection gets the rsockets
// default, which the files under /etc/rdma/rsocket (mem_default,
// wmem_default, sqsize_default, rqsize_default, inline_default) set for the
// whole process. A Dialer's Config applies to the socket it dials with, a
// ListenConfig's to the listening socket and, by inheritance, to every
// connection it accepts.
type Config struct {
	// Engine selects the engine that carries the connections: EngineNative,
	// also for the zero value, or EngineLibrdmacm. A Listener accepts
	// connections of its own engine, whatever the dialer runs.
	Engine Engine
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
}

// Engine names an engine, what carries the connections of a Dialer or a
// Listener; both speak the rsockets protocol, so either talks to the other
// and to any rsockets application.
type Engine string

const (
	// EngineNative runs the rsockets protocol in Go on the verbs and cm
	// packages and lends its registered buffers to ReadFrom and WriteTo. It
	// does not run on iWARP devices.
	EngineNative Engine = "native"
	// EngineLibrdmacm drives the rsockets of librdmacm.
	EngineLibrdmacm Engine = "librdmacm"
)

// errConfig is the cause of the *net.OpError reported for a Config that no
// socket could take.
var errConfig = errors.New("invalid Config")

// validate rejects an Engine that names no engine and negative fields before
// a socket is created.
func (c Config) validate() error {
	switch c.Engine {
	case "", EngineNative, EngineLibrdmacm:
	default:
		return fmt.Errorf("%w: Engine %q is neither %q nor %q", errConfig, c.Engine, EngineNative, EngineLibrdmacm)
	}
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
	return nil
}

// apply sets every non-zero field on s, which must not be connected or
// listening yet.
func (c Config) apply(s socket) error {
	for _, o := range []struct {
		value int
		set   func(int) error
	}{
		{c.SendBufferSize, s.SetSendBuffer},
		{c.ReceiveBufferSize, s.SetReceiveBuffer},
		{c.SendQueueSize, s.SetSendQueueSize},
		{c.ReceiveQueueSize, s.SetReceiveQueueSize},
		{c.MaxInline, s.SetInline},
	} {
		if o.value != 0 {
			if err := o.set(o.value); err != nil {
				return err
			}
		}
	}
	return nil
}
