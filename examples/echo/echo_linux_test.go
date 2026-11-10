//go:build linux && !android && (amd64 || arm64 || loong64 || ppc64le || riscv64 || s390x)

package main

import (
	"bytes"
	"net"
	"testing"

	"github.com/wzshiming/rdma"
	"github.com/wzshiming/rdma/internal/rdmatest"
)

// TestRXEEcho serves on the rdmatest device and sends a line through the
// client, which must print the echo.
func TestRXEEcho(t *testing.T) {
	dev := rdmatest.Device(t)
	ln, err := rdma.Listen("rdma", net.JoinHostPort(dev.IP.String(), "0"))
	if err != nil {
		t.Fatal(err)
	}
	defer ln.Close()
	go serve(ln)

	var out bytes.Buffer
	if err := send(ln.Addr().String(), "hello, rdma", &out); err != nil {
		t.Fatalf("send() error: %v", err)
	}
	if out.String() != "hello, rdma\n" {
		t.Errorf("send() printed %q, want %q", out.String(), "hello, rdma\n")
	}
}
