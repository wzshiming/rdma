package rdma

import (
	"bytes"
	"context"
	"errors"
	"io"
	"net"
	"net/netip"
	"runtime"
	"testing"
	"time"

	"github.com/wzshiming/rdma/cm"
)

// listenFake starts a Listener on the fake fabric.
func listenFake(t *testing.T, f *fakeFabric, cfg Config) *Listener {
	t.Helper()
	lc := ListenConfig{Config: cfg, fab: f}
	ln, err := lc.Listen(context.Background(), "rdma", "10.99.0.1:0")
	if err != nil {
		t.Fatalf("Listen() error: %v", err)
	}
	t.Cleanup(func() { ln.Close() })
	return ln
}

// dialFake dials addr on the fake fabric.
func dialFake(t *testing.T, f *fakeFabric, cfg Config, addr string) *Conn {
	t.Helper()
	d := Dialer{Config: cfg, fab: f}
	c, err := d.Dial("rdma", addr)
	if err != nil {
		t.Fatalf("Dial(%s) error: %v", addr, err)
	}
	t.Cleanup(func() { c.Close() })
	return c
}

// acceptAsync runs ln.Accept in the background.
func acceptAsync(ln *Listener) <-chan struct {
	c   net.Conn
	err error
} {
	ch := make(chan struct {
		c   net.Conn
		err error
	}, 1)
	go func() {
		c, err := ln.Accept()
		ch <- struct {
			c   net.Conn
			err error
		}{c, err}
	}()
	return ch
}

func TestDialAndListenOnFakeFabric(t *testing.T) {
	f := newFakeFabric(t)
	ln := listenFake(t, f, Config{})
	addr, ok := ln.Addr().(*Addr)
	if !ok || addr.Port == 0 || addr.IP.String() != "10.99.0.1" {
		t.Fatalf("Listener.Addr() = %v (%T), want *Addr 10.99.0.1 with a real port", ln.Addr(), ln.Addr())
	}
	accepted := acceptAsync(ln)
	client := dialFake(t, f, Config{}, addr.String())
	r := <-accepted
	if r.err != nil {
		t.Fatalf("Accept() error: %v", r.err)
	}
	server, ok := r.c.(*Conn)
	if !ok {
		t.Fatalf("Accept() returned a %T, want *Conn", r.c)
	}
	defer server.Close()
	if client.RemoteAddr().String() != addr.String() || server.LocalAddr().String() != addr.String() {
		t.Errorf("client remote %v, server local %v; want both %v", client.RemoteAddr(), server.LocalAddr(), addr)
	}
	if client.LocalAddr().String() != server.RemoteAddr().String() || client.LocalAddr().Network() != "rdma" {
		t.Errorf("client local %v, server remote %v; want the same rdma address", client.LocalAddr(), server.RemoteAddr())
	}
	go io.Copy(server, server)
	if _, err := client.Write([]byte("ping")); err != nil {
		t.Fatalf("Write error: %v", err)
	}
	buf := make([]byte, 4)
	if _, err := io.ReadFull(client, buf); err != nil || string(buf) != "ping" {
		t.Fatalf("echo = %q, %v; want %q", buf, err, "ping")
	}
}

func TestDialNegotiatesTheSmallerConfig(t *testing.T) {
	f := newFakeFabric(t)
	ln := listenFake(t, f, Config{MessageSize: 16384, ReceiveDepth: 32})
	accepted := acceptAsync(ln)
	client := dialFake(t, f, Config{MessageSize: 4096, ReceiveDepth: 64, SendDepth: 3}, ln.Addr().String())
	r := <-accepted
	if r.err != nil {
		t.Fatalf("Accept() error: %v", r.err)
	}
	server := r.c.(*Conn)
	defer server.Close()
	for name, c := range map[string]*Conn{"client": client, "server": server} {
		if c.msgSize != 4096 || c.recvDepth != 32 {
			t.Errorf("%s negotiated message size %d, receive depth %d; want 4096, 32", name, c.msgSize, c.recvDepth)
		}
	}
	if client.sendDepth != 3 || server.sendDepth != DefaultSendDepth {
		t.Errorf("send depths = %d, %d; want the local values 3 and %d", client.sendDepth, server.sendDepth, DefaultSendDepth)
	}
}

func TestListenerRejectsForeignVersionAndKeepsServing(t *testing.T) {
	f := newFakeFabric(t)
	ln := listenFake(t, f, Config{})
	accepted := acceptAsync(ln)

	// A dialer speaking the next protocol version is turned away with our
	// reject frame...
	cfg := Config{}.withDefaults()
	hello := encodeHello(cfg)
	hello[4] = protocolVersion + 1
	_, _, err := f.dial(context.Background(), netip.AddrPort{}, ln.addr.addrPort(), cfg, hello)
	var rej *cm.RejectedError
	if !errors.As(err, &rej) || len(rej.PrivateData) != cm.MaxRejectPrivateData || !bytes.Equal(rej.PrivateData[:rejectSize], encodeReject(rejectVersion)) {
		t.Fatalf("foreign dial error = %v, want a *cm.RejectedError carrying %x", err, encodeReject(rejectVersion))
	}
	// ...and something that is not our handshake at all likewise...
	_, _, err = f.dial(context.Background(), netip.AddrPort{}, ln.addr.addrPort(), cfg, []byte("GET / HTTP/1.0"))
	if !errors.As(err, &rej) || !bytes.Equal(rej.PrivateData[:rejectSize], encodeReject(rejectMalformed)) {
		t.Fatalf("malformed dial error = %v, want a *cm.RejectedError carrying %x", err, encodeReject(rejectMalformed))
	}
	// ...while the listener goes on serving well-behaved dialers.
	client := dialFake(t, f, Config{}, ln.Addr().String())
	r := <-accepted
	if r.err != nil {
		t.Fatalf("Accept() error: %v", r.err)
	}
	r.c.Close()
	client.Close()
}

func TestDialSurfacesAVersionRejectAsProtocolError(t *testing.T) {
	f := newFakeFabric(t)
	acc, err := f.listen(netip.AddrPortFrom(fakeIP, 0), 1)
	if err != nil {
		t.Fatal(err)
	}
	defer acc.close()
	go func() {
		req, err := acc.accept(context.Background())
		if err != nil {
			t.Errorf("fake accept: %v", err)
			return
		}
		frame := encodeReject(rejectVersion)
		frame[4] = protocolVersion + 1
		req.reject(frame)
	}()
	d := Dialer{fab: f}
	c, err := d.Dial("rdma", acc.addr().String())
	if err == nil {
		c.Close()
		t.Fatal("Dial() to a rejecting listener succeeded")
	}
	var opErr *net.OpError
	if !errors.As(err, &opErr) || opErr.Op != "dial" || !errors.Is(err, ErrProtocol) {
		t.Fatalf("Dial() error = %v, want a dial *net.OpError matching ErrProtocol", err)
	}
	if !errors.Is(err, errRefused) {
		t.Errorf("Dial() error = %v, want it to match ECONNREFUSED too, like any reject", err)
	}
}

func TestDialContextCancelReturnsContextError(t *testing.T) {
	f := newFakeFabric(t)
	ln := listenFake(t, f, Config{}) // never accepts
	ctx, cancel := context.WithCancel(context.Background())
	go func() {
		time.Sleep(50 * time.Millisecond)
		cancel()
	}()
	d := Dialer{fab: f}
	start := time.Now()
	c, err := d.DialContext(ctx, "rdma", ln.Addr().String())
	if err == nil {
		c.Close()
		t.Fatal("DialContext() succeeded without an accept")
	}
	var opErr *net.OpError
	if !errors.As(err, &opErr) || opErr.Op != "dial" || !errors.Is(err, context.Canceled) {
		t.Fatalf("DialContext() error = %v, want a dial *net.OpError matching context.Canceled", err)
	}
	if elapsed := time.Since(start); elapsed > 2*time.Second {
		t.Errorf("DialContext() took %v to notice the cancel", elapsed)
	}
}

func TestDialerTimeoutAndDeadline(t *testing.T) {
	f := newFakeFabric(t)
	ln := listenFake(t, f, Config{}) // never accepts
	for name, d := range map[string]Dialer{
		"timeout":  {Timeout: 50 * time.Millisecond, fab: f},
		"deadline": {Deadline: time.Now().Add(50 * time.Millisecond), fab: f},
	} {
		t.Run(name, func(t *testing.T) {
			_, err := d.Dial("rdma", ln.Addr().String())
			var opErr *net.OpError
			if !errors.As(err, &opErr) || !opErr.Timeout() {
				t.Fatalf("Dial() error = %v, want a *net.OpError with Timeout() true", err)
			}
		})
	}
}

func TestDialerLocalAddrPinsTheSource(t *testing.T) {
	f := newFakeFabric(t)
	ln := listenFake(t, f, Config{})
	accepted := acceptAsync(ln)
	d := Dialer{LocalAddr: &Addr{IP: net.IPv4(10, 99, 0, 7), Port: 1234}, fab: f}
	c, err := d.Dial("rdma", ln.Addr().String())
	if err != nil {
		t.Fatalf("Dial() error: %v", err)
	}
	defer c.Close()
	if c.LocalAddr().String() != "10.99.0.7:1234" {
		t.Errorf("LocalAddr() = %v, want 10.99.0.7:1234", c.LocalAddr())
	}
	r := <-accepted
	if r.err != nil {
		t.Fatalf("Accept() error: %v", r.err)
	}
	r.c.Close()

	wrong := Dialer{LocalAddr: &net.TCPAddr{IP: net.IPv4(10, 99, 0, 7)}, fab: f}
	_, err = wrong.Dial("rdma", ln.Addr().String())
	var addrErr *net.AddrError
	if !errors.As(err, &addrErr) {
		t.Errorf("Dial() with a TCP local address error = %v, want a *net.AddrError", err)
	}
}

func TestInvalidConfigIsReportedBeforeDialing(t *testing.T) {
	f := newFakeFabric(t)
	d := Dialer{Config: Config{MessageSize: 100}, fab: f}
	_, err := d.Dial("rdma", "10.99.0.1:1")
	var opErr *net.OpError
	if !errors.As(err, &opErr) || opErr.Op != "dial" || errors.Is(err, errRefused) {
		t.Errorf("Dial() with MessageSize 100 error = %v, want a dial *net.OpError about the Config", err)
	}
	lc := ListenConfig{Config: Config{ReceiveDepth: -1}, fab: f}
	_, err = lc.Listen(context.Background(), "rdma", "10.99.0.1:0")
	if !errors.As(err, &opErr) || opErr.Op != "listen" {
		t.Errorf("Listen() with ReceiveDepth -1 error = %v, want a listen *net.OpError", err)
	}
}

func TestListenWildcardAndUnknownNetwork(t *testing.T) {
	f := newFakeFabric(t)
	lc := ListenConfig{fab: f}
	ln, err := lc.Listen(context.Background(), "rdma", ":0")
	if err != nil {
		t.Fatalf("Listen(:0) error: %v", err)
	}
	defer ln.Close()
	if a := ln.Addr().(*Addr); !a.IP.IsUnspecified() || a.Port == 0 {
		t.Errorf("Listen(:0).Addr() = %v, want the unspecified address with a real port", a)
	}
	if _, err := lc.Listen(context.Background(), "tcp", ":0"); err == nil {
		t.Error("Listen(tcp) succeeded")
	}
	if _, err := Listen("rdma", "10.99.0.1:0"); err == nil && runtime.GOOS != "linux" {
		t.Error("Listen on the platform fabric succeeded without a device")
	}
}

func TestIdleReadDeadlineTimesOutWithinTolerance(t *testing.T) {
	f := newFakeFabric(t)
	a, b := fakePipe(t, f, Config{})
	defer a.Close()
	defer b.Close()
	const d = 50 * time.Millisecond
	a.SetReadDeadline(time.Now().Add(d))
	start := time.Now()
	n, err := a.Read(make([]byte, 1))
	elapsed := time.Since(start)
	if n != 0 || !errors.Is(err, context.DeadlineExceeded) && !isTimeout(err) {
		t.Fatalf("Read() = %d, %v; want 0 and a timeout", n, err)
	}
	if elapsed < d || elapsed > d+200*time.Millisecond {
		t.Errorf("Read() timed out after %v, want about %v", elapsed, d)
	}
}

func isTimeout(err error) bool {
	var nerr net.Error
	return errors.As(err, &nerr) && nerr.Timeout()
}

func TestCloseWakesBlockedReadAndWrite(t *testing.T) {
	f := newFakeFabric(t)
	a, b := fakePipe(t, f, Config{MessageSize: 256, ReceiveDepth: 1, SendDepth: 1})
	defer b.Close()
	readErr := make(chan error, 1)
	go func() {
		_, err := a.Read(make([]byte, 1))
		readErr <- err
	}()
	writeErr := make(chan error, 1)
	go func() {
		// The peer never reads, so the second message waits for a credit.
		_, err := a.Write(make([]byte, 1000))
		writeErr <- err
	}()
	time.Sleep(50 * time.Millisecond)
	if err := a.Close(); err != nil {
		t.Fatalf("Close() error: %v", err)
	}
	for name, ch := range map[string]chan error{"Read": readErr, "Write": writeErr} {
		select {
		case err := <-ch:
			if !errors.Is(err, net.ErrClosed) {
				t.Errorf("%s after Close() error = %v, want net.ErrClosed", name, err)
			}
		case <-time.After(5 * time.Second):
			t.Errorf("%s did not return after Close()", name)
		}
	}
}

func TestListenerCloseUnblocksAccept(t *testing.T) {
	f := newFakeFabric(t)
	ln := listenFake(t, f, Config{})
	accepted := acceptAsync(ln)
	time.Sleep(20 * time.Millisecond)
	if err := ln.Close(); err != nil {
		t.Fatalf("Close() error: %v", err)
	}
	select {
	case r := <-accepted:
		var opErr *net.OpError
		if !errors.As(r.err, &opErr) || opErr.Op != "accept" || !errors.Is(r.err, net.ErrClosed) {
			t.Errorf("Accept() after Close() = %v, %v; want an accept *net.OpError with net.ErrClosed", r.c, r.err)
		}
	case <-time.After(5 * time.Second):
		t.Fatal("Accept() did not return after Close()")
	}
	if _, err := ln.Accept(); !errors.Is(err, net.ErrClosed) {
		t.Errorf("second Accept() error = %v, want net.ErrClosed", err)
	}
	if err := ln.Close(); !errors.Is(err, net.ErrClosed) {
		t.Errorf("second Close() error = %v, want net.ErrClosed", err)
	}
}

func TestResourcesReturnToBaseline(t *testing.T) {
	f := newFakeFabric(t)
	ln := listenFake(t, f, Config{})
	before := runtime.NumGoroutine()
	const n = 16
	for range n {
		accepted := acceptAsync(ln)
		d := Dialer{fab: f}
		client, err := d.Dial("rdma", ln.Addr().String())
		if err != nil {
			t.Fatalf("Dial() error: %v", err)
		}
		r := <-accepted
		if r.err != nil {
			t.Fatalf("Accept() error: %v", r.err)
		}
		if _, err := client.Write([]byte("x")); err != nil {
			t.Fatalf("Write error: %v", err)
		}
		if _, err := r.c.Read(make([]byte, 1)); err != nil {
			t.Fatalf("Read error: %v", err)
		}
		if err := client.Close(); err != nil {
			t.Errorf("client Close() error: %v", err)
		}
		if err := r.c.Close(); err != nil {
			t.Errorf("server Close() error: %v", err)
		}
	}
	if got := f.registered.Load(); got != 0 {
		t.Errorf("%d bytes of buffer memory still registered after closing %d connections", got, n)
	}
	deadline := time.Now().Add(5 * time.Second)
	for runtime.NumGoroutine() > before && time.Now().Before(deadline) {
		time.Sleep(10 * time.Millisecond)
	}
	if after := runtime.NumGoroutine(); after > before {
		t.Errorf("goroutines: %d before, %d after closing %d connections", before, after, n)
	}
}
