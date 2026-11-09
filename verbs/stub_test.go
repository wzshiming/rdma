//go:build !linux || !cgo

package verbs_test

import (
	"errors"
	"testing"

	"github.com/wzshiming/rdma/verbs"
)

func TestStubUnsupported(t *testing.T) {
	ctx := verbs.ContextFromPointer(nil)
	tests := []struct {
		name string
		call func() error
	}{
		{"Devices", func() error { _, err := verbs.Devices(); return err }},
		{"ForkInit", verbs.ForkInit},
		{"Context.QueryDevice", func() error { _, err := ctx.QueryDevice(); return err }},
		{"Context.QueryPort", func() error { _, err := ctx.QueryPort(1); return err }},
		{"Context.GIDTable", func() error { _, err := ctx.GIDTable(1); return err }},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			err := tt.call()
			if !errors.Is(err, errors.ErrUnsupported) {
				t.Fatalf("err = %v, want errors.ErrUnsupported", err)
			}
			var opErr *verbs.OpError
			if !errors.As(err, &opErr) {
				t.Fatalf("err = %T, want *verbs.OpError", err)
			}
		})
	}
	if ctx.Pointer() != nil {
		t.Errorf("Pointer() = %v, want nil", ctx.Pointer())
	}
	if err := ctx.Close(); err != nil {
		t.Errorf("Close() = %v, want nil", err)
	}
}
