//go:build linux && !android && (amd64 || arm64 || loong64 || ppc64le || riscv64 || s390x)

package rsocket_test

import (
	"bytes"
	"errors"
	"io"
	"net/netip"
	"os"
	"syscall"
	"testing"
	"time"

	"github.com/wzshiming/rdma/internal/rdmatest"
	"github.com/wzshiming/rdma/rsocket"
	"github.com/wzshiming/rdma/verbs"
)

const timeout = 10 * time.Second

// newSocket creates an IPv4 socket closed when the test ends.
func newSocket(t *testing.T) *rsocket.Socket {
	t.Helper()
	s, err := rsocket.New(rsocket.IPv4)
	if err != nil {
		t.Fatalf("New() error: %v", err)
	}
	t.Cleanup(func() {
		if err := s.Close(); err != nil && !errors.Is(err, os.ErrClosed) {
			t.Errorf("Close() error: %v", err)
		}
	})
	return s
}

// listener binds a non-blocking listener to port 0 of the rdmatest device's
// IP, configured by setup first when not nil.
func listener(t *testing.T, setup func(*rsocket.Socket)) (rdmatest.Dev, *rsocket.Socket) {
	t.Helper()
	dev := rdmatest.Device(t)
	ln := newSocket(t)
	if setup != nil {
		setup(ln)
	}
	if err := ln.Bind(netip.AddrPortFrom(dev.IP, 0)); err != nil {
		t.Fatalf("Bind(%v:0) error: %v", dev.IP, err)
	}
	if err := ln.Listen(8); err != nil {
		t.Fatalf("Listen() error: %v", err)
	}
	if err := ln.SetNonblock(true); err != nil {
		t.Fatalf("SetNonblock(true) error: %v", err)
	}
	return dev, ln
}

// dial starts a non-blocking connect from the device's IP to remote, which
// reports EINPROGRESS; the attempt goes on as the socket is polled.
func dial(t *testing.T, dev rdmatest.Dev, remote netip.AddrPort, setup func(*rsocket.Socket)) *rsocket.Socket {
	t.Helper()
	c := newSocket(t)
	if setup != nil {
		setup(c)
	}
	if err := c.Bind(netip.AddrPortFrom(dev.IP, 0)); err != nil {
		t.Fatalf("Bind(%v:0) error: %v", dev.IP, err)
	}
	if err := c.SetNonblock(true); err != nil {
		t.Fatalf("SetNonblock(true) error: %v", err)
	}
	if err := c.Connect(remote); !errors.Is(err, syscall.EINPROGRESS) {
		t.Fatalf("non-blocking Connect(%v) error = %v, want EINPROGRESS", remote, err)
	}
	return c
}

func localAddr(t *testing.T, s *rsocket.Socket) netip.AddrPort {
	t.Helper()
	addr, err := s.LocalAddr()
	if err != nil {
		t.Fatalf("LocalAddr() error: %v", err)
	}
	return addr
}

// await polls fds until one of them reports an event, failing at deadline.
func await(t *testing.T, fds []rsocket.PollFD, deadline time.Time) {
	t.Helper()
	for i := range fds {
		fds[i].Revents = 0
	}
	n, err := rsocket.Poll(fds, time.Until(deadline))
	if err != nil {
		t.Fatalf("Poll(%v) error: %v", fds, err)
	}
	if n == 0 {
		t.Fatalf("Poll(%v) timed out", fds)
	}
}

// connect establishes a connection between a listener and a dialer on the
// device, both configured by setup, and returns them with the accepted
// socket, left as Accept returned it, and the peer address Accept reported.
func connect(t *testing.T, setup func(*rsocket.Socket)) (ln, client, server *rsocket.Socket, peer netip.AddrPort) {
	t.Helper()
	dev, ln := listener(t, setup)
	client = dial(t, dev, localAddr(t, ln), setup)
	deadline := time.Now().Add(timeout)
	// The request reaches the listener once the client's Poll has driven
	// its address and route resolution; the client connects once accepted.
	fds := []rsocket.PollFD{{Fd: ln.Descriptor(), Events: rsocket.PollIn}, {Fd: client.Descriptor(), Events: rsocket.PollOut}}
	for fds[0].Revents == 0 {
		await(t, fds, deadline)
		if fds[1].Revents&rsocket.PollErr != 0 {
			t.Fatalf("Connect() failed before Accept(): %v", client.SocketError())
		}
	}
	server, peer, err := ln.Accept()
	if err != nil {
		t.Fatalf("Accept() error: %v", err)
	}
	t.Cleanup(func() {
		if err := server.Close(); err != nil && !errors.Is(err, os.ErrClosed) {
			t.Errorf("Close() error: %v", err)
		}
	})
	await(t, fds[1:], deadline)
	if fds[1].Revents&rsocket.PollOut == 0 || fds[1].Revents&rsocket.PollErr != 0 {
		t.Fatalf("client Revents = %v after Accept(), want POLLOUT without POLLERR", fds[1].Revents)
	}
	if err := client.SocketError(); err != nil {
		t.Fatalf("SocketError() after connecting = %v, want nil", err)
	}
	return ln, client, server, peer
}

// settle makes an accepted socket non-blocking and waits until its
// handshake has completed, which it reports as writability.
func settle(t *testing.T, server *rsocket.Socket) {
	t.Helper()
	if err := server.SetNonblock(true); err != nil {
		t.Fatalf("SetNonblock(true) error: %v", err)
	}
	fds := []rsocket.PollFD{{Fd: server.Descriptor(), Events: rsocket.PollOut}}
	await(t, fds, time.Now().Add(timeout))
	if fds[0].Revents&rsocket.PollOut == 0 || fds[0].Revents&rsocket.PollErr != 0 {
		t.Fatalf("accepted socket Revents = %v, want POLLOUT without POLLERR", fds[0].Revents)
	}
}

// pair returns a connected, non-blocking client and server.
func pair(t *testing.T) (client, server *rsocket.Socket) {
	t.Helper()
	_, client, server, _ = connect(t, nil)
	settle(t, server)
	return client, server
}

// writeAll writes b to s, waiting for room whenever Write reports EAGAIN.
func writeAll(t *testing.T, s *rsocket.Socket, b []byte) {
	t.Helper()
	deadline := time.Now().Add(timeout)
	for len(b) > 0 {
		n, err := s.Write(b)
		if errors.Is(err, syscall.EAGAIN) {
			await(t, []rsocket.PollFD{{Fd: s.Descriptor(), Events: rsocket.PollOut}}, deadline)
			continue
		}
		if err != nil {
			t.Fatalf("Write() error: %v", err)
		}
		b = b[n:]
	}
}

// readFull fills b from s, waiting for data whenever Read reports EAGAIN.
func readFull(t *testing.T, s *rsocket.Socket, b []byte) {
	t.Helper()
	deadline := time.Now().Add(timeout)
	for len(b) > 0 {
		n, err := s.Read(b)
		if errors.Is(err, syscall.EAGAIN) {
			await(t, []rsocket.PollFD{{Fd: s.Descriptor(), Events: rsocket.PollIn}}, deadline)
			continue
		}
		if err != nil {
			t.Fatalf("Read() error: %v", err)
		}
		b = b[n:]
	}
}

// pattern returns n bytes that reveal a shifted or missing block.
func pattern(n int) []byte {
	b := make([]byte, n)
	for i := range b {
		b[i] = byte(i ^ i>>8 ^ i>>16)
	}
	return b
}

func TestListenOnPortZeroAssignsPort(t *testing.T) {
	dev, ln := listener(t, nil)
	addr := localAddr(t, ln)
	if addr.Addr() != dev.IP || addr.Port() == 0 {
		t.Fatalf("LocalAddr() = %v, want %v with a real port", addr, dev.IP)
	}
}

func TestAcceptWithoutRequestReturnsEAGAIN(t *testing.T) {
	_, ln := listener(t, nil)
	conn, peer, err := ln.Accept()
	var opErr *verbs.OpError
	if conn != nil || peer.IsValid() || !errors.Is(err, syscall.EAGAIN) || !errors.As(err, &opErr) || opErr.Op != "raccept" {
		t.Fatalf("idle Accept() = %v, %v, %#v; want nil, zero address and a *verbs.OpError{Op: \"raccept\", Err: EAGAIN}", conn, peer, err)
	}
}

func TestNonblockingConnectCompletesThroughPoll(t *testing.T) {
	// connect checks EINPROGRESS, the POLLOUT readiness and a nil SocketError.
	ln, client, server, peer := connect(t, nil)
	clientAddr := localAddr(t, client)
	if peer != clientAddr || clientAddr.Port() == 0 {
		t.Errorf("Accept() peer = %v, client LocalAddr() = %v; want the same address with a real port", peer, clientAddr)
	}
	if remote, err := client.RemoteAddr(); err != nil || remote != localAddr(t, ln) {
		t.Errorf("client RemoteAddr() = %v, %v; want %v", remote, err, localAddr(t, ln))
	}
	if remote, err := server.RemoteAddr(); err != nil || remote != clientAddr {
		t.Errorf("server RemoteAddr() = %v, %v; want %v", remote, err, clientAddr)
	}
	if local := localAddr(t, server); local != localAddr(t, ln) {
		t.Errorf("server LocalAddr() = %v, want the listener's %v", local, localAddr(t, ln))
	}
}

// TestAcceptedSocketNonblockIsExplicit measures whether an accepted socket
// inherits the listener's non-blocking flag and checks that SetNonblock
// makes it non-blocking either way.
func TestAcceptedSocketNonblockIsExplicit(t *testing.T) {
	_, client, server, _ := connect(t, nil)
	done := make(chan error, 1)
	go func() {
		_, err := server.Read(make([]byte, 1))
		done <- err
	}()
	select {
	case err := <-done:
		t.Logf("Read() on the accepted socket returned at once with %v: O_NONBLOCK inherited", err)
	case <-time.After(200 * time.Millisecond):
		t.Log("Read() on the accepted socket blocks: O_NONBLOCK not inherited")
		writeAll(t, client, []byte{1})
		if err := <-done; err != nil {
			t.Fatalf("blocking Read() error: %v", err)
		}
	}
	if err := server.SetNonblock(true); err != nil {
		t.Fatalf("SetNonblock(true) error: %v", err)
	}
	n, err := server.Read(make([]byte, 1))
	if n != 0 || !errors.Is(err, syscall.EAGAIN) {
		t.Fatalf("Read() after SetNonblock(true) = %d, %v; want 0, EAGAIN", n, err)
	}
}

func TestReadWriteRoundTrip(t *testing.T) {
	client, server := pair(t)
	msg := pattern(64 << 10)
	writeAll(t, client, msg)
	got := make([]byte, len(msg))
	readFull(t, server, got)
	if !bytes.Equal(got, msg) {
		t.Fatalf("the server read %d bytes differing from what the client wrote", len(got))
	}
	writeAll(t, server, []byte("pong"))
	reply := make([]byte, 4)
	readFull(t, client, reply)
	if string(reply) != "pong" {
		t.Fatalf("the client read %q, want %q", reply, "pong")
	}
}

func TestZeroLengthReadWriteAreNoOps(t *testing.T) {
	client, _ := pair(t)
	if n, err := client.Read(nil); n != 0 || err != nil {
		t.Errorf("Read(nil) = %d, %v; want 0, nil", n, err)
	}
	if n, err := client.Write(nil); n != 0 || err != nil {
		t.Errorf("Write(nil) = %d, %v; want 0, nil", n, err)
	}
}

func TestReadWithoutDataReturnsEAGAIN(t *testing.T) {
	_, server := pair(t)
	n, err := server.Read(make([]byte, 16))
	var opErr *verbs.OpError
	if n != 0 || !errors.Is(err, syscall.EAGAIN) || !errors.As(err, &opErr) || opErr.Op != "rrecv" {
		t.Fatalf("Read() without data = %d, %#v; want 0 and a *verbs.OpError{Op: \"rrecv\", Err: EAGAIN}", n, err)
	}
}

func TestWriteWithoutRoomReturnsEAGAIN(t *testing.T) {
	client, server := pair(t)
	// Nobody reads, so the writes fill the buffers of both sides, partially
	// at the end, until nothing fits.
	const limit = 8 << 20
	chunk := pattern(32 << 10)
	var sent []byte
	for len(sent) < limit {
		n, err := client.Write(chunk)
		if errors.Is(err, syscall.EAGAIN) {
			if n != 0 {
				t.Fatalf("Write() = %d with EAGAIN, want 0", n)
			}
			break
		}
		if err != nil {
			t.Fatalf("Write() error: %v", err)
		}
		sent = append(sent, chunk[:n]...)
	}
	if len(sent) >= limit {
		t.Fatalf("wrote %d bytes without EAGAIN", len(sent))
	}
	t.Logf("%d bytes buffered before EAGAIN", len(sent))
	got := make([]byte, len(sent))
	readFull(t, server, got)
	if !bytes.Equal(got, sent) {
		t.Fatal("the server read differs from what the client wrote")
	}
	// Drained, the client can write again once the server has returned the
	// buffer space, which it does from a call that services its socket: the
	// Poll on it here, as an event loop would make.
	fds := []rsocket.PollFD{{Fd: client.Descriptor(), Events: rsocket.PollOut}, {Fd: server.Descriptor(), Events: rsocket.PollIn}}
	await(t, fds, time.Now().Add(timeout))
	if fds[0].Revents&rsocket.PollOut == 0 || fds[1].Revents != 0 {
		t.Fatalf("Revents = %v, %v after the drain; want POLLOUT on the client only", fds[0].Revents, fds[1].Revents)
	}
}

func TestShutdownWriteGivesPeerEOF(t *testing.T) {
	client, server := pair(t)
	writeAll(t, client, []byte("bye"))
	if err := client.Shutdown(rsocket.ShutWrite); err != nil {
		t.Fatalf("Shutdown(ShutWrite) error: %v", err)
	}
	got := make([]byte, 3)
	readFull(t, server, got)
	if string(got) != "bye" {
		t.Fatalf("the server read %q, want %q", got, "bye")
	}
	deadline := time.Now().Add(timeout)
	for {
		n, err := server.Read(make([]byte, 16))
		if errors.Is(err, syscall.EAGAIN) {
			await(t, []rsocket.PollFD{{Fd: server.Descriptor(), Events: rsocket.PollIn}}, deadline)
			continue
		}
		if n != 0 || err != io.EOF {
			t.Fatalf("Read() after the peer's shutdown = %d, %v; want 0, io.EOF", n, err)
		}
		break
	}
	// The other direction stays open.
	writeAll(t, server, []byte("ok"))
	reply := make([]byte, 2)
	readFull(t, client, reply)
	if string(reply) != "ok" {
		t.Fatalf("the client read %q after its shutdown, want %q", reply, "ok")
	}
}

func TestConnectWithoutListenerFails(t *testing.T) {
	dev, ln := listener(t, nil)
	addr := localAddr(t, ln)
	if err := ln.Close(); err != nil {
		t.Fatalf("Close() error: %v", err)
	}
	client := dial(t, dev, addr, nil)
	fds := []rsocket.PollFD{{Fd: client.Descriptor(), Events: rsocket.PollOut}}
	await(t, fds, time.Now().Add(timeout))
	err := client.SocketError()
	var opErr *verbs.OpError
	var errno syscall.Errno
	if !errors.As(err, &opErr) || opErr.Op != "rconnect" || !errors.As(err, &errno) || errno == 0 {
		t.Fatalf("SocketError() = %#v with Revents %v; want a *verbs.OpError{Op: \"rconnect\", Err: errno}", err, fds[0].Revents)
	}
	t.Logf("connecting to %v without a listener: Revents %v, SocketError() = %v (errno %d)", addr, fds[0].Revents, err, int(errno))
	if errno != syscall.ECONNREFUSED {
		t.Logf("the errno is not ECONNREFUSED")
	}
	if fds[0].Revents&rsocket.PollErr == 0 {
		t.Errorf("Revents = %v, want POLLERR for a failed connect", fds[0].Revents)
	}
}

func TestOptionsSetBeforeConnect(t *testing.T) {
	setup := func(s *rsocket.Socket) {
		for _, opt := range []struct {
			name string
			err  error
		}{
			{"SetSendBuffer", s.SetSendBuffer(256 << 10)},
			{"SetReceiveBuffer", s.SetReceiveBuffer(256 << 10)},
			{"SetSendQueueSize", s.SetSendQueueSize(128)},
			{"SetReceiveQueueSize", s.SetReceiveQueueSize(128)},
			{"SetInline", s.SetInline(128)},
			{"SetReuseAddr", s.SetReuseAddr(true)},
			{"SetV6Only", s.SetV6Only(false)},
		} {
			if opt.err != nil {
				t.Fatalf("%s error: %v", opt.name, opt.err)
			}
		}
	}
	_, client, server, _ := connect(t, setup)
	settle(t, server)
	msg := pattern(16 << 10)
	writeAll(t, client, msg)
	got := make([]byte, len(msg))
	readFull(t, server, got)
	if !bytes.Equal(got, msg) {
		t.Fatalf("the server read %d bytes differing from what the client wrote", len(got))
	}
	writeAll(t, server, msg)
	readFull(t, client, got)
	if !bytes.Equal(got, msg) {
		t.Fatalf("the client read %d bytes differing from what the server wrote", len(got))
	}
}

func TestPollMixesSocketsAndPipes(t *testing.T) {
	client, server := pair(t)
	r, w := pipe(t)
	fds := []rsocket.PollFD{{Fd: server.Descriptor(), Events: rsocket.PollIn}, {Fd: r, Events: rsocket.PollIn}}
	if _, err := syscall.Write(w, []byte{1}); err != nil {
		t.Fatalf("Write() to the pipe error: %v", err)
	}
	await(t, fds, time.Now().Add(timeout))
	if fds[0].Revents != 0 || fds[1].Revents&rsocket.PollIn == 0 {
		t.Fatalf("Revents = %v, %v; want only the pipe readable", fds[0].Revents, fds[1].Revents)
	}
	if _, err := syscall.Read(r, make([]byte, 1)); err != nil {
		t.Fatalf("Read() from the pipe error: %v", err)
	}
	writeAll(t, client, []byte{1})
	await(t, fds, time.Now().Add(timeout))
	if fds[0].Revents&rsocket.PollIn == 0 || fds[1].Revents != 0 {
		t.Fatalf("Revents = %v, %v; want only the socket readable", fds[0].Revents, fds[1].Revents)
	}
}

func TestClosedSocketReportsErrClosed(t *testing.T) {
	rdmatest.Device(t)
	s := newSocket(t)
	if fd := s.Descriptor(); fd < 0 {
		t.Fatalf("Descriptor() = %d, want a descriptor", fd)
	}
	if err := s.Close(); err != nil {
		t.Fatalf("Close() error: %v", err)
	}
	addr := netip.MustParseAddrPort("10.200.0.1:7471")
	tests := []struct {
		name string
		op   string
		call func() error
	}{
		{"Bind", "rbind", func() error { return s.Bind(addr) }},
		{"Listen", "rlisten", func() error { return s.Listen(1) }},
		{"Accept", "raccept", func() error { _, _, err := s.Accept(); return err }},
		{"Connect", "rconnect", func() error { return s.Connect(addr) }},
		{"SocketError", "rgetsockopt", func() error { return s.SocketError() }},
		{"Read", "rrecv", func() error { _, err := s.Read(make([]byte, 1)); return err }},
		{"Write", "rsend", func() error { _, err := s.Write([]byte{1}); return err }},
		{"Shutdown", "rshutdown", func() error { return s.Shutdown(rsocket.ShutWrite) }},
		{"Close", "rclose", func() error { return s.Close() }},
		{"LocalAddr", "rgetsockname", func() error { _, err := s.LocalAddr(); return err }},
		{"RemoteAddr", "rgetpeername", func() error { _, err := s.RemoteAddr(); return err }},
		{"SetSendBuffer", "rsetsockopt", func() error { return s.SetSendBuffer(1 << 16) }},
		{"SetReceiveBuffer", "rsetsockopt", func() error { return s.SetReceiveBuffer(1 << 16) }},
		{"SetSendQueueSize", "rsetsockopt", func() error { return s.SetSendQueueSize(64) }},
		{"SetReceiveQueueSize", "rsetsockopt", func() error { return s.SetReceiveQueueSize(64) }},
		{"SetInline", "rsetsockopt", func() error { return s.SetInline(64) }},
		{"SetReuseAddr", "rsetsockopt", func() error { return s.SetReuseAddr(true) }},
		{"SetV6Only", "rsetsockopt", func() error { return s.SetV6Only(true) }},
		{"SetNonblock", "rfcntl", func() error { return s.SetNonblock(true) }},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			err := tt.call()
			var opErr *verbs.OpError
			if !errors.Is(err, os.ErrClosed) || !errors.As(err, &opErr) || opErr.Op != tt.op {
				t.Fatalf("%s after Close() error = %#v, want a *verbs.OpError{Op: %q, Err: os.ErrClosed}", tt.name, err, tt.op)
			}
		})
	}
	if fd := s.Descriptor(); fd != -1 {
		t.Errorf("Descriptor() after Close() = %d, want -1", fd)
	}
}
