//go:build !linux || !cgo

package rdma_test

import (
	"context"
	"errors"
	"net"
	"testing"

	"github.com/wzshiming/rdma"
)

// TestStubUnsupported checks that without librdmacm the net-style
// constructors fail the way the net package would report it.
func TestStubUnsupported(t *testing.T) {
	tests := []struct {
		name string
		op   string
		call func() error
	}{
		{"Dial", "dial", func() error { _, err := rdma.Dial("rdma", "10.200.0.1:4791"); return err }},
		{"DialContext", "dial", func() error {
			_, err := rdma.DialContext(context.Background(), "rdma4", "10.200.0.1:4791")
			return err
		}},
		{"Dialer", "dial", func() error {
			d := rdma.Dialer{Config: rdma.Config{SendBufferSize: 4096}}
			_, err := d.Dial("rdma", "10.200.0.1:4791")
			return err
		}},
		{"Listen", "listen", func() error { _, err := rdma.Listen("rdma", "10.200.0.1:0"); return err }},
		{"ListenConfig", "listen", func() error {
			lc := rdma.ListenConfig{Backlog: 4}
			_, err := lc.Listen(context.Background(), "rdma", ":0")
			return err
		}},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			err := tt.call()
			var opErr *net.OpError
			if !errors.As(err, &opErr) || opErr.Op != tt.op || opErr.Net != "rdma" {
				t.Fatalf("err = %v, want a *net.OpError with Op %q on net rdma", err, tt.op)
			}
			if !errors.Is(err, errors.ErrUnsupported) {
				t.Errorf("errors.Is(err, errors.ErrUnsupported) = false for %v", err)
			}
		})
	}
}
