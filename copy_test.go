package rdma

import (
	"net"
	"testing"

	"golang.org/x/net/nettest"
)

// engineUnderTest is an engine the copy-path suites run on.
type engineUnderTest struct {
	name string
	// new returns a fresh engine for t.
	new func(t testing.TB) engine
	// addr is the address to listen on, host:port.
	addr string
	// owns reports whether p lies in the buffers of s; nil for an engine
	// whose streams do not lend.
	owns func(s stream, p []byte) bool
}

// fakeEngines are the two forms of the fake engine.
var fakeEngines = []engineUnderTest{
	{name: "lending", new: newLendingFake, addr: "10.99.0.1:0", owns: fakeOwns},
	{name: "copying", new: newCopyingFake, addr: "10.99.0.1:0"},
}

// pair connects a dialer to a listener on a fresh engine of e, both with cfg,
// and returns the two ends.
func (e engineUnderTest) pair(t testing.TB, cfg Config) (client, server *Conn) {
	t.Helper()
	eng := e.new(t)
	lc := ListenConfig{Config: cfg, eng: eng}
	ln, err := lc.Listen(t.Context(), "rdma", e.addr)
	if err != nil {
		t.Fatalf("Listen(%s) error: %v", e.addr, err)
	}
	defer ln.Close()
	acc := acceptAsync(ln)
	d := Dialer{Config: cfg, eng: eng}
	client, err = d.Dial("rdma", ln.Addr().String())
	if err != nil {
		t.Fatalf("Dial(%v) error: %v", ln.Addr(), err)
	}
	t.Cleanup(func() { client.Close() })
	r := <-acc
	if r.err != nil {
		t.Fatalf("AcceptRDMA() error: %v", r.err)
	}
	t.Cleanup(func() { r.c.Close() })
	return client, r.c
}

// testConn runs nettest.TestConn on pairs of e.
func testConn(t *testing.T, e engineUnderTest) {
	nettest.TestConn(t, func() (c1, c2 net.Conn, stop func(), err error) {
		client, server := e.pair(t, Config{})
		return client, server, func() {
			client.Close()
			server.Close()
		}, nil
	})
}

func TestConnOnFakeEngines(t *testing.T) {
	for _, e := range fakeEngines {
		t.Run(e.name, func(t *testing.T) { testConn(t, e) })
	}
}
