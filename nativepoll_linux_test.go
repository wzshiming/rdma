//go:build linux && !android && (amd64 || arm64 || loong64 || ppc64le || riscv64 || s390x)

package rdma

import (
	"testing"
	"time"
)

// The native engine has the dialed and the accepted stream poll for the
// PollTime of their Config, and neither of them poll for zero.
func TestRXENativeStreamsPollForThePollTime(t *testing.T) {
	for _, poll := range []time.Duration{0, 3 * time.Microsecond} {
		client, server := rxeEnginePair(t, Config{PollTime: poll}, EngineNative, EngineNative)
		for _, c := range []*Conn{client, server} {
			if got := pollTimeOf(c); got != poll {
				t.Errorf("with PollTime %v a native stream polls for %v, want %v", poll, got, poll)
			}
		}
	}
}

// pollTimeOf returns how long the waits of c, a native connection, poll: 0
// when they do not.
func pollTimeOf(c *Conn) time.Duration {
	if p := c.s.(*nativeStream).poller; p != nil {
		return p.time
	}
	return 0
}
