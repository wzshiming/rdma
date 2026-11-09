//go:build !plan9

package rdma

import (
	"errors"
	"net"
	"os"
	"syscall"
	"testing"
	"time"
)

// TestErrorShapes pins the error contract on the fake fabric: every failure
// is a *net.OpError on net "rdma" with the right Op, and matches the
// sentinel a net package user would test for.
func TestErrorShapes(t *testing.T) {
	f := newFakeFabric(t)
	ln := listenFake(t, f, Config{})
	addr := ln.Addr().String()
	accepted := acceptAsync(ln)
	client := dialFake(t, f, Config{}, addr)
	r := <-accepted
	if r.err != nil {
		t.Fatalf("Accept() error: %v", r.err)
	}
	server := r.c.(*Conn)
	defer server.Close()

	refusedAddr := func() string {
		dead := listenFake(t, f, Config{})
		a := dead.Addr().String()
		dead.Close()
		return a
	}()
	closedLn := listenFake(t, f, Config{})
	closedLn.Close()

	timedOut, _ := fakePipe(t, f, Config{})
	defer timedOut.Close()
	timedOut.SetDeadline(time.Now().Add(-time.Second))

	shut, shutPeer := fakePipe(t, f, Config{})
	defer shutPeer.Close()
	defer shut.Close()
	if err := shut.CloseWrite(); err != nil {
		t.Fatalf("CloseWrite() error: %v", err)
	}

	closed, closedPeer := fakePipe(t, f, Config{})
	defer closedPeer.Close()
	if err := closed.Close(); err != nil {
		t.Fatalf("Close() error: %v", err)
	}

	reset, resetPeer := fakePipe(t, f, Config{})
	defer reset.Close()
	resetPeer.t.close() // the peer vanished without sending EOF

	tests := []struct {
		name    string
		call    func() error
		op      string
		target  error
		timeout bool
	}{
		{"dial refused", func() error { _, err := (&Dialer{fab: f}).Dial("rdma", refusedAddr); return err }, "dial", syscall.ECONNREFUSED, false},
		{"accept closed", func() error { _, err := closedLn.Accept(); return err }, "accept", net.ErrClosed, false},
		{"read closed", func() error { _, err := closed.Read(make([]byte, 1)); return err }, "read", net.ErrClosed, false},
		{"write closed", func() error { _, err := closed.Write([]byte("x")); return err }, "write", net.ErrClosed, false},
		{"close closed", func() error { return closed.Close() }, "close", net.ErrClosed, false},
		{"set deadline closed", func() error { return closed.SetDeadline(time.Time{}) }, "set", net.ErrClosed, false},
		{"read timeout", func() error { _, err := timedOut.Read(make([]byte, 1)); return err }, "read", os.ErrDeadlineExceeded, true},
		{"write timeout", func() error { _, err := timedOut.Write([]byte("x")); return err }, "write", os.ErrDeadlineExceeded, true},
		{"write after CloseWrite", func() error { _, err := shut.Write([]byte("x")); return err }, "write", syscall.EPIPE, false},
		{"read from vanished peer", func() error { _, err := reset.Read(make([]byte, 1)); return err }, "read", syscall.ECONNRESET, false},
		{"write to vanished peer", func() error { _, err := reset.Write([]byte("x")); return err }, "write", syscall.ECONNRESET, false},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			err := tt.call()
			var opErr *net.OpError
			if !errors.As(err, &opErr) {
				t.Fatalf("error = %T %v, want *net.OpError", err, err)
			}
			if opErr.Op != tt.op || opErr.Net != "rdma" {
				t.Errorf("OpError{Op: %q, Net: %q}, want Op %q on net rdma", opErr.Op, opErr.Net, tt.op)
			}
			if !errors.Is(err, tt.target) {
				t.Errorf("errors.Is(%v, %v) = false", err, tt.target)
			}
			if os.IsTimeout(err) != tt.timeout || opErr.Timeout() != tt.timeout {
				t.Errorf("os.IsTimeout = %v, Timeout() = %v; want %v", os.IsTimeout(err), opErr.Timeout(), tt.timeout)
			}
		})
	}
	// The vanished peer's EOF would have made Read return io.EOF instead.
	if _, err := client.Write([]byte("still fine")); err != nil {
		t.Errorf("Write on the healthy connection error: %v", err)
	}
}
