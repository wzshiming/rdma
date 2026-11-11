//go:build linux && !android && (amd64 || arm64 || loong64 || ppc64le || riscv64 || s390x)

package rdma

import (
	"context"
	"errors"
	"io"
	"net"
	"net/netip"
	"runtime"
	"syscall"
	"testing"
	"time"

	"github.com/wzshiming/rdma/cm"
	"github.com/wzshiming/rdma/internal/rdmatest"
	"github.com/wzshiming/rdma/verbs"
)

// rxeEnginePair connects a Dialer of the engine dial to a fresh listener of
// the engine listen on the device, both with cfg otherwise, and closes the
// listener. The dialed end must run on dial and the accepted one on listen,
// its listener's engine whatever the dialer's.
func rxeEnginePair(t testing.TB, cfg Config, dial, listen Engine) (client, server *Conn) {
	t.Helper()
	lcfg := cfg
	lcfg.Engine = listen
	ln := rxeListen(t, lcfg)
	defer ln.Close()
	acc := acceptAsync(ln)
	d := Dialer{Config: cfg, Timeout: rxeTimeout}
	d.Config.Engine = dial
	client, err := d.Dial("rdma", ln.Addr().String())
	if err != nil {
		t.Fatalf("Dial(%v) error: %v", ln.Addr(), err)
	}
	t.Cleanup(func() { client.Close() })
	r := <-acc
	if r.err != nil {
		t.Fatalf("AcceptRDMA() error: %v", r.err)
	}
	t.Cleanup(func() { r.c.Close() })
	if got := engineOf(client); got != dial {
		t.Fatalf("the dialed connection runs on the %q engine, want %q", got, dial)
	}
	if got := engineOf(r.c); got != listen {
		t.Fatalf("the accepted connection runs on the %q engine, want the listener's %q", got, listen)
	}
	return client, r.c
}

// engineOf returns the engine that carries c, "" for none of the platform's.
func engineOf(c *Conn) Engine {
	switch c.s.(type) {
	case *nativeStream:
		return EngineNative
	case *rstream:
		return EngineLibrdmacm
	}
	return ""
}

// A native Dialer and a listener of the librdmacm engine move 64 MiB each way
// at once through io.Copy.
func TestRXENativeDialerCopy(t *testing.T) {
	testRXECopy(t, Config{}, EngineNative, EngineLibrdmacm, 64<<20)
}

// A native Dialer runs the exchange of rstream against its server.
func TestRXENativeDialerAgainstRstreamServer(t *testing.T) {
	testRXEDialerAgainstRstreamServer(t, EngineNative)
}

// A native dial refuses a device whose transport is iWARP before it reaches
// the peer, and connects on any other.
func TestRXENativeDialRefusesIWARP(t *testing.T) {
	dev := rdmatest.Device(t)
	for _, tt := range []struct {
		transport verbs.TransportType
		refused   bool
	}{
		{verbs.TransportIB, false},
		{verbs.TransportIWARP, true},
	} {
		t.Run(tt.transport.String(), func(t *testing.T) {
			lc := ListenConfig{Config: Config{Engine: EngineLibrdmacm}}
			ln, err := lc.Listen(context.Background(), "rdma", net.JoinHostPort(dev.IP.String(), "0"))
			if err != nil {
				t.Fatalf("Listen(%v:0) error: %v", dev.IP, err)
			}
			defer ln.Close()
			acc := acceptAsync(ln)
			eng := nativeEngine{transportType: func(*verbs.Context) verbs.TransportType { return tt.transport }}
			d := Dialer{Timeout: rxeTimeout, eng: eng}
			c, err := d.Dial("rdma", ln.Addr().String())
			if !tt.refused {
				if err != nil {
					t.Fatalf("Dial(%v) on a device of %v error: %v", ln.Addr(), tt.transport, err)
				}
				c.Close()
				if r := <-acc; r.err == nil {
					r.c.Close()
				}
				return
			}
			if err == nil {
				c.Close()
				t.Fatalf("Dial(%v) on a device of %v succeeded, want it refused", ln.Addr(), tt.transport)
			}
			if !isOpError(err, "dial", errors.ErrUnsupported) {
				t.Errorf("Dial(%v) on a device of %v error = %v, want a dial *net.OpError matching errors.ErrUnsupported", ln.Addr(), tt.transport, err)
			}
			ln.Close()
			if r := <-acc; r.err == nil {
				r.c.Close()
				t.Error("the listener accepted a connection from the refused dial")
			}
		})
	}
}

// Closed, the connections of the native engine leave no goroutine behind.
func TestRXENativeDialerGoroutinesReturnToBaseline(t *testing.T) {
	testRXEGoroutinesReturnToBaseline(t, EngineNative, EngineLibrdmacm)
}

// testRXEGoroutinesReturnToBaseline connects a few pairs of a Dialer of the
// engine dial and a listener of the engine listen, moves a byte each way on
// each and closes both ends, and checks that no goroutine is left behind.
func testRXEGoroutinesReturnToBaseline(t *testing.T, dial, listen Engine) {
	rdmatest.Device(t)
	goroutines := runtime.NumGoroutine()
	const n = 4
	for range n {
		client, server := rxeEnginePair(t, Config{}, dial, listen)
		buf := make([]byte, 1)
		for _, ends := range [][2]*Conn{{client, server}, {server, client}} {
			if _, err := ends[0].Write([]byte("x")); err != nil {
				t.Fatalf("Write error: %v", err)
			}
			if _, err := io.ReadFull(ends[1], buf); err != nil {
				t.Fatalf("ReadFull error: %v", err)
			}
		}
		if err := client.Close(); err != nil {
			t.Errorf("client Close() error: %v", err)
		}
		if err := server.Close(); err != nil {
			t.Errorf("server Close() error: %v", err)
		}
	}
	deadline := time.Now().Add(5 * time.Second)
	for runtime.NumGoroutine() > goroutines && time.Now().Before(deadline) {
		time.Sleep(10 * time.Millisecond)
	}
	if after := runtime.NumGoroutine(); after > goroutines {
		t.Errorf("goroutines: %d before, %d after closing %d connections", goroutines, after, n)
	}
}

// peerDialers are the engines whose Dialers a native listener is tested
// against.
var peerDialers = []struct {
	name string
	eng  Engine
}{
	{"librdmacm dialer", EngineLibrdmacm},
	{"native dialer", EngineNative},
}

// A native listener and a Dialer of either engine move 64 MiB each way at
// once through io.Copy.
func TestRXENativeListenerCopy(t *testing.T) {
	for _, dial := range peerDialers {
		t.Run(dial.name, func(t *testing.T) {
			testRXECopy(t, Config{}, dial.eng, EngineNative, 64<<20)
		})
	}
}

// rstream's client runs the exchange against a native listener.
func TestRXERstreamClientAgainstNativeListener(t *testing.T) {
	testRXERstreamClientAgainstListener(t, EngineNative)
}

// A native listener rejects the requests that arrive on an iWARP device: the
// Dialer of either engine is refused with ECONNREFUSED and Accept never
// returns them.
func TestRXENativeListenerRejectsIWARP(t *testing.T) {
	iwarp := nativeEngine{transportType: func(*verbs.Context) verbs.TransportType { return verbs.TransportIWARP }}
	for _, dial := range peerDialers {
		t.Run(dial.name, func(t *testing.T) {
			ln := rxeListenOn(t, Config{}, iwarp)
			acc := acceptAsync(ln)
			d := Dialer{Config: Config{Engine: dial.eng}, Timeout: rxeTimeout}
			c, err := d.Dial("rdma", ln.Addr().String())
			if err == nil {
				c.Close()
				t.Fatalf("Dial(%v) to a listener on an iWARP device succeeded, want it refused", ln.Addr())
			}
			if !isOpError(err, "dial", syscall.ECONNREFUSED) {
				t.Errorf("Dial(%v) to a listener on an iWARP device error = %v, want a dial *net.OpError matching ECONNREFUSED", ln.Addr(), err)
			}
			checkNothingAccepted(t, ln, acc)
		})
	}
}

// A native listener rejects a request whose connection data is of a version
// other than 1: the connect is refused with ECONNREFUSED and Accept never
// returns it.
func TestRXENativeListenerRejectsOtherVersions(t *testing.T) {
	ln := rxeListen(t, Config{Engine: EngineNative})
	acc := acceptAsync(ln)
	ctx, cancel := context.WithTimeout(context.Background(), rxeTimeout)
	defer cancel()
	id, err := cm.Resolve(ctx, netip.AddrPort{}, ln.Addr().(*Addr).addrPort())
	if err != nil {
		t.Fatalf("Resolve(%v) error: %v", ln.Addr(), err)
	}
	// A native stream sets the queue pair up that the request comes from.
	s, err := newQPStream(id, Config{})
	if err != nil {
		t.Fatalf("newQPStream error: %v", err)
	}
	defer s.close()
	data := s.connData().Encode()
	data[0] = 2
	if _, err := id.Connect(ctx, &cm.ConnParam{PrivateData: data[:]}); !errors.Is(err, syscall.ECONNREFUSED) {
		t.Errorf("Connect(%v) with connection data of version 2 error = %v, want ECONNREFUSED", ln.Addr(), err)
	}
	checkNothingAccepted(t, ln, acc)
}

// checkNothingAccepted closes ln, on which acc waits for an Accept, and fails
// the test when that Accept returned a connection.
func checkNothingAccepted(t *testing.T, ln *Listener, acc <-chan accepted) {
	t.Helper()
	ln.Close()
	if r := <-acc; r.err == nil {
		r.c.Close()
		t.Error("Accept returned the connection of a rejected request")
	}
}

// Closed, a native listener and the connections it accepted leave no
// goroutine behind.
func TestRXENativeListenerGoroutinesReturnToBaseline(t *testing.T) {
	for _, dial := range peerDialers {
		t.Run(dial.name, func(t *testing.T) {
			testRXEGoroutinesReturnToBaseline(t, dial.eng, EngineNative)
		})
	}
}
