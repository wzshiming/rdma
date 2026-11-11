package rdma

import (
	"context"
	"errors"
	"io"
	"net"
	"runtime"
	"strconv"
	"strings"
	"testing"
	"time"
)

func TestDialAndListenOnFakeNetwork(t *testing.T) {
	nw := newFakeNet(t)
	ln := listenFake(t, nw, Config{})
	addr, ok := ln.Addr().(*Addr)
	if !ok || addr.Port == 0 || addr.IP.String() != "10.99.0.1" {
		t.Fatalf("Listener.Addr() = %v (%T), want *Addr 10.99.0.1 with a real port", ln.Addr(), ln.Addr())
	}
	acc := acceptAsync(ln)
	client := dialFake(t, nw, Config{}, addr.String())
	r := <-acc
	if r.err != nil {
		t.Fatalf("AcceptRDMA() error: %v", r.err)
	}
	server := r.c
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

// TestConfigReachesTheSockets checks that every Config field is set on the
// dialing socket and on the listening socket, which the accepted socket
// inherits, and that zero fields are left alone.
func TestConfigReachesTheSockets(t *testing.T) {
	nw := newFakeNet(t)
	full := Config{SendBufferSize: 1 << 16, ReceiveBufferSize: 1 << 17, SendQueueSize: 128, ReceiveQueueSize: 256, MaxInline: 128}
	want := fakeOptions{sndbuf: 1 << 16, rcvbuf: 1 << 17, sqsize: 128, rqsize: 256, inline: 128}
	ln := listenFake(t, nw, full)
	if got := nw.options(ln.a); got != want {
		t.Errorf("listening socket options = %+v, want %+v", got, want)
	}
	acc := acceptAsync(ln)
	client := dialFake(t, nw, Config{SendBufferSize: 4096}, ln.Addr().String())
	if got := nw.options(client.s); got != (fakeOptions{sndbuf: 4096}) {
		t.Errorf("dialing socket options = %+v, want only sndbuf 4096", got)
	}
	r := <-acc
	if r.err != nil {
		t.Fatalf("AcceptRDMA() error: %v", r.err)
	}
	defer r.c.Close()
	if got := nw.options(r.c.s); got != want {
		t.Errorf("accepted socket options = %+v, want the listener's %+v", got, want)
	}
}

func TestDialRefusedWithoutListener(t *testing.T) {
	nw := newFakeNet(t)
	dead := listenFake(t, nw, Config{})
	addr := dead.Addr().String()
	dead.Close()
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	d := Dialer{nw: nw}
	c, err := d.DialContext(ctx, "rdma", addr)
	if err == nil {
		c.Close()
		t.Fatalf("DialContext(%s) succeeded without a listener", addr)
	}
	var opErr *net.OpError
	if !errors.As(err, &opErr) || opErr.Op != "dial" || !errors.Is(err, errRefused) {
		t.Fatalf("DialContext(%s) error = %v, want a dial *net.OpError with ECONNREFUSED", addr, err)
	}
	if opErr.Addr == nil || opErr.Addr.String() != addr {
		t.Errorf("OpError.Addr = %v, want %s", opErr.Addr, addr)
	}
	if nw.openSockets() != 0 {
		t.Errorf("%d sockets left open by the refused dial", nw.openSockets())
	}
}

func TestDialContextCancelReturnsContextError(t *testing.T) {
	nw := newFakeNet(t)
	goroutines := runtime.NumGoroutine()
	ctx, cancel := context.WithCancel(context.Background())
	go func() {
		time.Sleep(50 * time.Millisecond)
		cancel()
	}()
	d := Dialer{nw: nw}
	start := time.Now()
	c, err := d.DialContext(ctx, "rdma", fakeNowhere)
	if err == nil {
		c.Close()
		t.Fatal("DialContext() to an unanswered host succeeded")
	}
	var opErr *net.OpError
	if !errors.As(err, &opErr) || opErr.Op != "dial" || !errors.Is(err, context.Canceled) {
		t.Fatalf("DialContext() error = %v, want a dial *net.OpError matching context.Canceled", err)
	}
	if elapsed := time.Since(start); elapsed > 2*time.Second {
		t.Errorf("DialContext() took %v to notice the cancel", elapsed)
	}
	if open := nw.openSockets(); open != 0 {
		t.Errorf("%d sockets open after the cancelled dial, want 0", open)
	}
	deadline := time.Now().Add(5 * time.Second)
	for runtime.NumGoroutine() > goroutines && time.Now().Before(deadline) {
		time.Sleep(10 * time.Millisecond)
	}
	if after := runtime.NumGoroutine(); after > goroutines {
		t.Errorf("goroutines: %d before the dial, %d after its cancel", goroutines, after)
	}
}

func TestDialerTimeoutAndDeadline(t *testing.T) {
	nw := newFakeNet(t)
	for name, d := range map[string]Dialer{
		"timeout":  {Timeout: 50 * time.Millisecond, nw: nw},
		"deadline": {Deadline: time.Now().Add(50 * time.Millisecond), nw: nw},
	} {
		t.Run(name, func(t *testing.T) {
			_, err := d.Dial("rdma", fakeNowhere)
			var opErr *net.OpError
			if !errors.As(err, &opErr) || !opErr.Timeout() {
				t.Fatalf("Dial() error = %v, want a *net.OpError with Timeout() true", err)
			}
		})
	}
}

func TestDialerLocalAddrPinsTheSource(t *testing.T) {
	nw := newFakeNet(t)
	ln := listenFake(t, nw, Config{})
	acc := acceptAsync(ln)
	d := Dialer{LocalAddr: &Addr{IP: net.IPv4(10, 99, 0, 7), Port: 1234}, nw: nw}
	c, err := d.Dial("rdma", ln.Addr().String())
	if err != nil {
		t.Fatalf("Dial() error: %v", err)
	}
	defer c.Close()
	if c.LocalAddr().String() != "10.99.0.7:1234" {
		t.Errorf("LocalAddr() = %v, want 10.99.0.7:1234", c.LocalAddr())
	}
	r := <-acc
	if r.err != nil {
		t.Fatalf("AcceptRDMA() error: %v", r.err)
	}
	defer r.c.Close()
	if r.c.RemoteAddr().String() != "10.99.0.7:1234" {
		t.Errorf("peer RemoteAddr() = %v, want the pinned 10.99.0.7:1234", r.c.RemoteAddr())
	}

	wrong := Dialer{LocalAddr: &net.TCPAddr{IP: net.IPv4(10, 99, 0, 7)}, nw: nw}
	_, err = wrong.Dial("rdma", ln.Addr().String())
	var addrErr *net.AddrError
	if !errors.As(err, &addrErr) {
		t.Errorf("Dial() with a TCP local address error = %v, want a *net.AddrError", err)
	}
}

func TestNegativeConfigIsReportedBeforeDialing(t *testing.T) {
	nw := newFakeNet(t)
	for _, cfg := range []Config{
		{SendBufferSize: -1},
		{ReceiveBufferSize: -1},
		{SendQueueSize: -1},
		{ReceiveQueueSize: -1},
		{MaxInline: -1},
	} {
		d := Dialer{Config: cfg, nw: nw}
		_, err := d.Dial("rdma", "10.99.0.1:1")
		var opErr *net.OpError
		if !errors.As(err, &opErr) || opErr.Op != "dial" || errors.Is(err, errRefused) {
			t.Errorf("Dial() with %+v error = %v, want a dial *net.OpError about the Config", cfg, err)
		}
		lc := ListenConfig{Config: cfg, nw: nw}
		if _, err = lc.Listen(context.Background(), "rdma", "10.99.0.1:0"); !errors.As(err, &opErr) || opErr.Op != "listen" {
			t.Errorf("Listen() with %+v error = %v, want a listen *net.OpError", cfg, err)
		}
	}
	if open := nw.openSockets(); open != 0 {
		t.Errorf("%d sockets created for configurations rejected up front", open)
	}
}

func TestListenWildcardAndUnknownNetwork(t *testing.T) {
	nw := newFakeNet(t)
	lc := ListenConfig{nw: nw}
	ln, err := lc.Listen(context.Background(), "rdma", ":0")
	if err != nil {
		t.Fatalf("Listen(:0) error: %v", err)
	}
	defer ln.Close()
	if a := ln.Addr().(*Addr); !a.IP.IsUnspecified() || a.Port == 0 {
		t.Errorf("Listen(:0).Addr() = %v, want the unspecified address with a real port", a)
	}
	// A wildcard listener takes dials to any address of its port.
	acc := acceptAsync(ln)
	c := dialFake(t, nw, Config{}, net.JoinHostPort(fakeIP.String(), strconv.Itoa(ln.Addr().(*Addr).Port)))
	defer c.Close()
	if r := <-acc; r.err != nil {
		t.Errorf("AcceptRDMA() on the wildcard listener error: %v", r.err)
	} else {
		r.c.Close()
	}
	if _, err := lc.Listen(context.Background(), "tcp", ":0"); err == nil {
		t.Error("Listen(tcp) succeeded")
	}
	if _, err := Listen("rdma", "10.99.0.1:0"); err == nil && runtime.GOOS != "linux" {
		t.Error("Listen on the platform network succeeded without a device")
	}
}

func TestListenerCloseUnblocksAccept(t *testing.T) {
	nw := newFakeNet(t)
	ln := listenFake(t, nw, Config{})
	acc := acceptAsync(ln)
	time.Sleep(20 * time.Millisecond)
	if err := ln.Close(); err != nil {
		t.Fatalf("Close() error: %v", err)
	}
	select {
	case r := <-acc:
		var opErr *net.OpError
		if !errors.As(r.err, &opErr) || opErr.Op != "accept" || !errors.Is(r.err, net.ErrClosed) {
			t.Errorf("AcceptRDMA() after Close() = %v, %v; want an accept *net.OpError with net.ErrClosed", r.c, r.err)
		}
	case <-time.After(5 * time.Second):
		t.Fatal("AcceptRDMA() did not return after Close()")
	}
	if _, err := ln.Accept(); !errors.Is(err, net.ErrClosed) {
		t.Errorf("second Accept() error = %v, want net.ErrClosed", err)
	}
	if err := ln.Close(); !errors.Is(err, net.ErrClosed) {
		t.Errorf("second Close() error = %v, want net.ErrClosed", err)
	}
}

// TestDialErrorsNameTheirCause checks the messages of the failures a user
// sees first.
func TestDialErrorsNameTheirCause(t *testing.T) {
	nw := newFakeNet(t)
	d := Dialer{Config: Config{MaxInline: -1}, nw: nw}
	_, err := d.Dial("rdma", "10.99.0.1:1")
	if err == nil || !strings.Contains(err.Error(), "MaxInline") {
		t.Errorf("Dial() with MaxInline -1 error = %v, want it to name the field", err)
	}
	_, err = d.Dial("rdma", ":1")
	if err == nil || !strings.Contains(err.Error(), "missing IP address") {
		t.Errorf("Dial() without a host error = %v, want \"missing IP address\"", err)
	}
}
