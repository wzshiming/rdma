//go:build linux && !android && (amd64 || arm64 || loong64 || ppc64le || riscv64 || s390x)

package rdma

import (
	"context"
	"errors"
	"io"
	"net"
	"runtime"
	"testing"
	"time"

	"github.com/wzshiming/rdma/internal/rdmatest"
	"github.com/wzshiming/rdma/verbs"
)

// rxeNativeDialPair connects a Dialer of the native engine to a fresh
// listener of the librdmacm engine on the device, both with cfg, and closes
// the listener.
func rxeNativeDialPair(t testing.TB, cfg Config) (native, peer *Conn) {
	t.Helper()
	return rxeEnginePair(t, cfg, nativeDialer{}, librdmacmEngine{&rsocketNet})
}

// rxeEnginePair connects a Dialer of the engine dial to a fresh listener of
// the engine listen on the device, both with cfg, and closes the listener.
func rxeEnginePair(t testing.TB, cfg Config, dial, listen engine) (client, server *Conn) {
	t.Helper()
	ln := rxeListenOn(t, cfg, listen)
	defer ln.Close()
	acc := acceptAsync(ln)
	d := Dialer{Config: cfg, Timeout: rxeTimeout, eng: dial}
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
	return client, r.c
}

// A native Dialer interoperates with a listener of the librdmacm engine, its
// end playing either role of the exchanges: client, which writes, shuts down
// writing or closes first, and server.
func TestRXENativeDialer(t *testing.T) {
	for _, tt := range []struct {
		name string
		cfg  Config
		test func(t *testing.T, client, server *Conn)
	}{
		{"Echo", smallConfig, testRXEEcho},
		{"CloseWriteLetsThePeerWriteBack", Config{}, testRXECloseWriteLetsThePeerWriteBack},
		{"BufferedDataPrecedesEOF", Config{}, testRXEBufferedDataPrecedesEOF},
	} {
		t.Run(tt.name, func(t *testing.T) {
			t.Run("native client", func(t *testing.T) {
				native, peer := rxeNativeDialPair(t, tt.cfg)
				tt.test(t, native, peer)
			})
			t.Run("native server", func(t *testing.T) {
				native, peer := rxeNativeDialPair(t, tt.cfg)
				tt.test(t, peer, native)
			})
		})
	}
}

// A native Dialer and a listener of the librdmacm engine move 64 MiB each way
// at once through io.Copy.
func TestRXENativeDialerCopy(t *testing.T) {
	testRXECopy(t, nativeDialer{}, librdmacmEngine{&rsocketNet}, 64<<20)
}

// A native Dialer runs the exchange of rstream against its server.
func TestRXENativeDialerAgainstRstreamServer(t *testing.T) {
	testRXEDialerAgainstRstreamServer(t, nativeDialer{})
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
			lc := ListenConfig{eng: librdmacmEngine{&rsocketNet}}
			ln, err := lc.Listen(context.Background(), "rdma", net.JoinHostPort(dev.IP.String(), "0"))
			if err != nil {
				t.Fatalf("Listen(%v:0) error: %v", dev.IP, err)
			}
			defer ln.Close()
			acc := acceptAsync(ln)
			eng := nativeDialer{nativeEngine{transportType: func(*verbs.Context) verbs.TransportType { return tt.transport }}}
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
	rdmatest.Device(t)
	goroutines := runtime.NumGoroutine()
	const n = 4
	for range n {
		native, peer := rxeNativeDialPair(t, Config{})
		buf := make([]byte, 1)
		for _, ends := range [][2]*Conn{{native, peer}, {peer, native}} {
			if _, err := ends[0].Write([]byte("x")); err != nil {
				t.Fatalf("Write error: %v", err)
			}
			if _, err := io.ReadFull(ends[1], buf); err != nil {
				t.Fatalf("ReadFull error: %v", err)
			}
		}
		if err := native.Close(); err != nil {
			t.Errorf("native Close() error: %v", err)
		}
		if err := peer.Close(); err != nil {
			t.Errorf("peer Close() error: %v", err)
		}
	}
	deadline := time.Now().Add(5 * time.Second)
	for runtime.NumGoroutine() > goroutines && time.Now().Before(deadline) {
		time.Sleep(10 * time.Millisecond)
	}
	if after := runtime.NumGoroutine(); after > goroutines {
		t.Errorf("goroutines: %d before, %d after closing %d native connections", goroutines, after, n)
	}
}
