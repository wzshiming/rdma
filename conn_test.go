package rdma

import (
	"net"
	"net/netip"
	"testing"

	"golang.org/x/net/nettest"
)

// fakePipeMaker adapts fakePipe to nettest.
func fakePipeMaker(t *testing.T, cfg Config) nettest.MakePipe {
	return func() (c1, c2 net.Conn, stop func(), err error) {
		f := newFakeFabric(t)
		client, server := fakePipe(t, f, cfg)
		return client, server, func() {
			client.Close()
			server.Close()
		}, nil
	}
}

func TestConnOnFakeTransport(t *testing.T) {
	configs := map[string]Config{
		"defaults":   {},
		"depth1":     {MessageSize: 1024, ReceiveDepth: 1, SendDepth: 1},
		"tiny":       {MessageSize: 256, ReceiveDepth: 2, SendDepth: 3},
		"asymmetric": {MessageSize: 4096, ReceiveDepth: 64, SendDepth: 4},
	}
	for name, cfg := range configs {
		t.Run(name, func(t *testing.T) {
			nettest.TestConn(t, fakePipeMaker(t, cfg))
		})
	}
}

func TestFakeTransportFailsWithoutPostedBuffer(t *testing.T) {
	var failures []string
	f := newFakeFabric(t)
	f.fail = func(format string, args ...any) { failures = append(failures, format) }
	cfg := Config{MessageSize: 256, ReceiveDepth: 1, SendDepth: 1}
	a := newFakeTransport(f, cfg, netip.MustParseAddrPort("10.99.0.1:1"), netip.MustParseAddrPort("10.99.0.1:2"))
	b := newFakeTransport(f, cfg, netip.MustParseAddrPort("10.99.0.1:2"), netip.MustParseAddrPort("10.99.0.1:1"))
	a.link(b)
	b.link(a)
	// b posted ReceiveDepth+1 buffers; one message more is the violation.
	for i := 0; i <= cfg.ReceiveDepth+1; i++ {
		if err := a.postSend(0, 16); err != nil {
			t.Fatalf("postSend #%d error: %v", i, err)
		}
	}
	if len(failures) != 1 {
		t.Fatalf("the fake reported %d failures for one message too many, want 1: %q", len(failures), failures)
	}
}
