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

// TestErrorShapes pins the error contract on the fake network: every failure
// is a *net.OpError on net "rdma" with the right Op and addresses, and
// matches the sentinel a net package user would test for.
func TestErrorShapes(t *testing.T) {
	nw := newFakeNet(t)
	ln := listenFake(t, nw, Config{})
	addr := ln.Addr().String()
	acc := acceptAsync(ln)
	client := dialFake(t, nw, Config{}, addr)
	r := <-acc
	if r.err != nil {
		t.Fatalf("AcceptRDMA() error: %v", r.err)
	}
	server := r.c
	defer server.Close()

	refusedAddr := func() string {
		dead := listenFake(t, nw, Config{})
		a := dead.Addr().String()
		dead.Close()
		return a
	}()
	closedLn := listenFake(t, nw, Config{})
	closedLn.Close()

	timedOut, _ := fakePipe(t, nw, Config{})
	timedOut.SetDeadline(time.Now().Add(-time.Second))

	shut, _ := fakePipe(t, nw, Config{})
	if err := shut.CloseWrite(); err != nil {
		t.Fatalf("CloseWrite() error: %v", err)
	}

	closed, _ := fakePipe(t, nw, Config{})
	if err := closed.Close(); err != nil {
		t.Fatalf("Close() error: %v", err)
	}

	tests := []struct {
		name    string
		call    func() error
		op      string
		addr    string // the OpError's Addr, "" for nil
		target  error
		timeout bool
	}{
		{"dial refused", func() error { _, err := (&Dialer{nw: nw}).Dial("rdma", refusedAddr); return err }, "dial", refusedAddr, syscall.ECONNREFUSED, false},
		{"dial negative config", func() error {
			_, err := (&Dialer{Config: Config{SendQueueSize: -1}, nw: nw}).Dial("rdma", addr)
			return err
		}, "dial", addr, errConfig, false},
		{"listen negative config", func() error {
			lc := ListenConfig{Config: Config{ReceiveBufferSize: -1}, nw: nw}
			_, err := lc.Listen(t.Context(), "rdma", "10.99.0.1:0")
			return err
		}, "listen", "10.99.0.1:0", errConfig, false},
		{"accept closed", func() error { _, err := closedLn.Accept(); return err }, "accept", closedLn.Addr().String(), net.ErrClosed, false},
		{"listener close closed", func() error { return closedLn.Close() }, "close", closedLn.Addr().String(), net.ErrClosed, false},
		{"read closed", func() error { _, err := closed.Read(make([]byte, 1)); return err }, "read", closed.RemoteAddr().String(), net.ErrClosed, false},
		{"write closed", func() error { _, err := closed.Write([]byte("x")); return err }, "write", closed.RemoteAddr().String(), net.ErrClosed, false},
		{"close closed", func() error { return closed.Close() }, "close", closed.RemoteAddr().String(), net.ErrClosed, false},
		{"close write closed", func() error { return closed.CloseWrite() }, "close", closed.RemoteAddr().String(), net.ErrClosed, false},
		{"set deadline closed", func() error { return closed.SetDeadline(time.Time{}) }, "set", closed.RemoteAddr().String(), net.ErrClosed, false},
		{"read timeout", func() error { _, err := timedOut.Read(make([]byte, 1)); return err }, "read", timedOut.RemoteAddr().String(), os.ErrDeadlineExceeded, true},
		{"write timeout", func() error { _, err := timedOut.Write([]byte("x")); return err }, "write", timedOut.RemoteAddr().String(), os.ErrDeadlineExceeded, true},
		{"write after CloseWrite", func() error { _, err := shut.Write([]byte("x")); return err }, "write", shut.RemoteAddr().String(), syscall.EPIPE, false},
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
			if got := addrString(opErr.Addr); got != tt.addr {
				t.Errorf("OpError.Addr = %q, want %q", got, tt.addr)
			}
			if !errors.Is(err, tt.target) {
				t.Errorf("errors.Is(%v, %v) = false", err, tt.target)
			}
			if os.IsTimeout(err) != tt.timeout || opErr.Timeout() != tt.timeout {
				t.Errorf("os.IsTimeout = %v, Timeout() = %v; want %v", os.IsTimeout(err), opErr.Timeout(), tt.timeout)
			}
		})
	}
	// The connection the table did not touch still works.
	if _, err := client.Write([]byte("still fine")); err != nil {
		t.Errorf("Write on the healthy connection error: %v", err)
	}
	// Source and Addr of a connection's errors are its two ends.
	_, err := closed.Read(make([]byte, 1))
	var opErr *net.OpError
	if !errors.As(err, &opErr) || addrString(opErr.Source) != closed.LocalAddr().String() {
		t.Errorf("read error Source = %v, want the local address %v", opErr.Source, closed.LocalAddr())
	}
}

func addrString(a net.Addr) string {
	if a == nil {
		return ""
	}
	return a.String()
}
