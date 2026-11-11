//go:build linux && !android && (amd64 || arm64 || loong64 || ppc64le || riscv64 || s390x)

package rdma

import (
	"bytes"
	"context"
	"errors"
	"io"
	"net"
	"net/netip"
	"runtime"
	"sync"
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

// qpTransportOf returns the transport under c, a connection of the native
// engine.
func qpTransportOf(c *Conn) *qpTransport { return c.s.(*nativeStream).t.(*qpTransport) }

// registeredMemory returns the memory c, a connection of the native engine,
// registered with its device for its buffers: the send buffer, with the
// control slots behind it, and the receive buffer.
func registeredMemory(c *Conn) (send, recv []byte) {
	t := qpTransportOf(c)
	return t.smr.Bytes(), t.rmr.Bytes()
}

// ReadFrom hands its source and WriteTo its destination nothing but views of
// the memory the connection registered, while 64 MiB go each way at once
// between two native connections.
func TestRXENativeCopyLendsRegisteredMemory(t *testing.T) {
	rdmatest.Device(t)
	for _, tt := range []struct {
		name string
		cfg  Config
	}{
		{"defaults", Config{}},
		{"small", smallConfig},
	} {
		t.Run(tt.name, func(t *testing.T) {
			client, server := rxeEnginePair(t, tt.cfg, EngineNative, EngineNative)
			testCopyLendsRegisteredMemory(t, client, server, 64<<20, registeredMemory)
		})
	}
}

// testCopyLendsRegisteredMemory moves total bytes each way at once between
// client and server with io.Copy, which enters ReadFrom on the sending ends
// and WriteTo on the receiving ones, and checks the hashes, and that every
// buffer ReadFrom hands its source lies in the send memory of its end and
// every buffer WriteTo hands its destination in the receive memory of its
// own, as registered reports them.
func testCopyLendsRegisteredMemory(t *testing.T, client, server *Conn, total int, registered func(*Conn) (send, recv []byte)) {
	var wg sync.WaitGroup
	var ins, outs [2]*testStream
	var reads, writes, foreignReads, foreignWrites [2]int
	for i, ends := range [][2]*Conn{{client, server}, {server, client}} {
		ins[i], outs[i] = newStream(byte(i)), newStream(byte(i))
		send, _ := registered(ends[0])
		_, recv := registered(ends[1])
		wg.Add(2)
		go func() {
			defer wg.Done()
			src := &fullSource{s: ins[i], left: total}
			_, err := io.Copy(ends[0], readerFunc(func(p []byte) (int, error) {
				reads[i]++
				if !within(p, send) {
					foreignReads[i]++
				}
				return src.Read(p)
			}))
			if err != nil {
				t.Errorf("io.Copy(conn, src) #%d error: %v", i, err)
			}
			if err := ends[0].CloseWrite(); err != nil {
				t.Errorf("CloseWrite #%d error: %v", i, err)
			}
		}()
		go func() {
			defer wg.Done()
			dst := &sink{s: outs[i]}
			_, err := io.Copy(writerFunc(func(p []byte) (int, error) {
				writes[i]++
				if !within(p, recv) {
					foreignWrites[i]++
				}
				return dst.Write(p)
			}), ends[1])
			if err != nil {
				t.Errorf("io.Copy(dst, conn) #%d error: %v", i, err)
			}
		}()
	}
	wg.Wait()
	for i := range ins {
		t.Logf("direction #%d: ReadFrom handed the source %d buffers, WriteTo the destination %d", i, reads[i], writes[i])
		if foreignReads[i] > 0 || foreignWrites[i] > 0 {
			t.Errorf("direction #%d: %d of the source's buffers lie outside the registered send memory, %d of the destination's outside the receive memory", i, foreignReads[i], foreignWrites[i])
		}
		if ins[i].sum() != outs[i].sum() {
			t.Errorf("direction #%d: sent %s, received %s", i, ins[i].sum(), outs[i].sum())
		}
	}
}

// memoryReleased reports whether c, a connection of the native engine, has
// released the memory it registered.
func memoryReleased(c *Conn) bool {
	t := qpTransportOf(c)
	return t.smr.Bytes() == nil && t.rmr.Bytes() == nil
}

// Closed while its ReadFrom's source sits in Read, or its WriteTo's
// destination in Write, a native connection keeps the memory of their view
// for them to go on using, and releases it once the view is back.
func TestRXENativeViewsOutliveClose(t *testing.T) {
	rdmatest.Device(t)
	testViewsOutliveClose(t, func(t *testing.T) (client, server *Conn) {
		return rxeEnginePair(t, Config{}, EngineNative, EngineNative)
	}, memoryReleased)
}

// testViewsOutliveClose closes a connection of pair while its ReadFrom's
// source sits in Read with a view of the send buffer, and one while its
// WriteTo's destination sits in Write with a view of the receive buffer.
// Close returns with the view out and leaves its memory alone; the source
// then fills every byte of its view, the destination reads every byte of its
// own, which still holds what the peer wrote, the copy ends with
// net.ErrClosed, and released reports the memory gone.
func testViewsOutliveClose(t *testing.T, pair func(*testing.T) (client, server *Conn), released func(*Conn) bool) {
	t.Run("source in Read", func(t *testing.T) {
		client, _ := pair(t)
		entered, resume := make(chan struct{}), make(chan struct{})
		readFrom := async(func() (int64, error) {
			return client.ReadFrom(readerFunc(func(p []byte) (int, error) {
				close(entered)
				<-resume
				for i := range p {
					p[i] = byte(i)
				}
				return len(p), nil
			}))
		})
		<-entered
		closeWithAViewOut(t, client, released)
		close(resume)
		if r := await(t, "ReadFrom", readFrom); r.n != 0 || !isOpError(r.err, "readfrom", net.ErrClosed) {
			t.Errorf("ReadFrom() = %d, %v; want 0 and a readfrom *net.OpError with net.ErrClosed", r.n, r.err)
		}
		if !released(client) {
			t.Error("the memory was kept once the source's view was back")
		}
	})
	t.Run("destination in Write", func(t *testing.T) {
		client, server := pair(t)
		want := make([]byte, 32<<10)
		newStream(19).rng.Read(want)
		if _, err := server.Write(want); err != nil {
			t.Fatalf("Write error: %v", err)
		}
		entered, resume := make(chan struct{}), make(chan struct{})
		var held int
		intact := false
		writeTo := async(func() (int64, error) {
			return client.WriteTo(writerFunc(func(p []byte) (int, error) {
				close(entered)
				<-resume
				held, intact = len(p), bytes.Equal(p, want[:len(p)])
				return len(p), nil
			}))
		})
		<-entered
		closeWithAViewOut(t, client, released)
		close(resume)
		if r := await(t, "WriteTo", writeTo); r.n != int64(held) || !isOpError(r.err, "writeto", net.ErrClosed) {
			t.Errorf("WriteTo() = %d, %v; want the %d bytes of the view and a writeto *net.OpError with net.ErrClosed", r.n, r.err, held)
		}
		if !intact {
			t.Error("after Close the destination's view no longer held what the peer wrote")
		}
		if !released(client) {
			t.Error("the memory was kept once the destination's view was back")
		}
	})
}

// closeWithAViewOut closes c, which has a view out, and checks that Close
// returns and leaves the memory of the view alone.
func closeWithAViewOut(t *testing.T, c *Conn, released func(*Conn) bool) {
	t.Helper()
	var err error
	within5s(t, "Close", func() { err = c.Close() })
	if err != nil {
		t.Errorf("Close error: %v", err)
	}
	if released(c) {
		t.Error("Close released the memory of the view out")
	}
}

// With its peer reading nothing, a native connection that filled the room
// the peer granted it closes within closeTimeout, and the peer, of either
// engine and on either end of the connection, then reads everything written
// and io.EOF.
func TestRXENativeCloseWithThePeerNotReading(t *testing.T) {
	rdmatest.Device(t)
	forEachEngine(t, func(t *testing.T, peer Engine) {
		t.Run("native end dialed", func(t *testing.T) {
			closer, other := rxeEnginePair(t, Config{}, EngineNative, peer)
			testCloseWithThePeerNotReading(t, closer, other)
		})
		t.Run("native end accepted", func(t *testing.T) {
			other, closer := rxeEnginePair(t, Config{}, peer, EngineNative)
			testCloseWithThePeerNotReading(t, closer, other)
		})
	})
}

// testCloseWithThePeerNotReading has closer write until it waits for room
// its peer, which reads nothing, does not grant, and close: Close returns
// within closeTimeout, and the peer then reads all that was written and
// io.EOF.
func testCloseWithThePeerNotReading(t *testing.T, closer, peer *Conn) {
	in := newStream(17)
	chunk := make([]byte, 16<<10)
	closer.SetWriteDeadline(time.Now().Add(200 * time.Millisecond))
	for {
		in.rng.Read(chunk)
		n, err := closer.Write(chunk)
		in.hash.Write(chunk[:n])
		in.n += int64(n)
		if isTimeout(err) {
			break
		}
		if err != nil {
			t.Fatalf("Write error: %v", err)
		}
	}
	if in.n == 0 {
		t.Fatal("nothing was written before the write deadline")
	}
	start := time.Now()
	if err := closer.Close(); err != nil {
		t.Fatalf("Close error: %v", err)
	}
	elapsed := time.Since(start)
	t.Logf("Close with %d bytes the peer had not read took %v", in.n, elapsed.Round(time.Millisecond))
	if elapsed >= closeTimeout {
		t.Errorf("Close took %v with the peer not reading, want less than %v", elapsed, closeTimeout)
	}
	peer.SetReadDeadline(time.Now().Add(rxeTimeout))
	out := newStream(0)
	if err := out.readFrom(peer, 64<<10); err != nil {
		t.Errorf("peer Read after %d bytes error: %v", out.n, err)
	}
	if in.sum() != out.sum() {
		t.Errorf("wrote %s before Close, the peer read %s", in.sum(), out.sum())
	}
}
