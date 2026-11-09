//go:build !linux || !cgo

package verbs_test

import (
	"errors"
	"testing"

	"github.com/wzshiming/rdma/verbs"
)

func TestStubUnsupported(t *testing.T) {
	ctx := verbs.ContextFromPointer(nil)
	pd := verbs.PDFromPointer(nil)
	tests := []struct {
		name string
		call func() error
	}{
		{"Devices", func() error { _, err := verbs.Devices(); return err }},
		{"ForkInit", verbs.ForkInit},
		{"Context.QueryDevice", func() error { _, err := ctx.QueryDevice(); return err }},
		{"Context.QueryPort", func() error { _, err := ctx.QueryPort(1); return err }},
		{"Context.GIDTable", func() error { _, err := ctx.GIDTable(1); return err }},
		{"Context.AllocPD", func() error { _, err := ctx.AllocPD(); return err }},
		{"PD.RegisterMemory", func() error { _, err := pd.RegisterMemory(make([]byte, 16), verbs.AccessLocalWrite); return err }},
		{"PD.AllocMemory", func() error { _, err := pd.AllocMemory(4096, verbs.AccessLocalWrite); return err }},
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
	if err := pd.Close(); err != nil {
		t.Errorf("PD.Close() = %v, want nil", err)
	}
	mr := verbs.MRFromPointer(nil)
	if b := mr.Bytes(); b != nil {
		t.Errorf("MR.Bytes() = %v, want nil", b)
	}
	if err := mr.Close(); err != nil {
		t.Errorf("MR.Close() = %v, want nil", err)
	}
}
