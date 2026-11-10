//go:build !linux || android || !(amd64 || arm64 || loong64 || ppc64le || riscv64 || s390x)

package cm_test

import (
	"context"
	"errors"
	"net/netip"
	"testing"

	"github.com/wzshiming/rdma/cm"
	"github.com/wzshiming/rdma/verbs"
)

func TestStubUnsupported(t *testing.T) {
	ctx := context.Background()
	id := new(cm.ID)
	ln := new(cm.Listener)
	req := &cm.Request{ID: id}
	tests := []struct {
		name string
		call func() error
	}{
		{"Resolve", func() error { _, err := cm.Resolve(ctx, netip.AddrPort{}, v4); return err }},
		{"Listen", func() error { _, err := cm.Listen(netip.AddrPortFrom(v4.Addr(), 0), 1); return err }},
		{"ID.CreateQP", func() error {
			_, err := id.CreateQP(verbs.PDFromPointer(nil), &verbs.QPInitAttr{Type: verbs.QPTypeRC})
			return err
		}},
		{"ID.Connect", func() error { _, err := id.Connect(ctx, nil); return err }},
		{"ID.Disconnect", func() error { return id.Disconnect(ctx) }},
		{"ID.Wait", func() error { _, err := id.Wait(ctx); return err }},
		{"Listener.Accept", func() error { _, err := ln.Accept(ctx); return err }},
		{"Request.Accept", func() error { return req.Accept(ctx, &cm.ConnParam{PrivateData: []byte("ok")}) }},
		{"Request.Reject", func() error { return req.Reject(nil) }},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			err := tt.call()
			if !errors.Is(err, errors.ErrUnsupported) {
				t.Fatalf("err = %v, want errors.ErrUnsupported", err)
			}
			var opErr *verbs.OpError
			if !errors.As(err, &opErr) || opErr.Op == "" {
				t.Fatalf("err = %T, want *verbs.OpError naming the rdma_cm function", err)
			}
		})
	}
	if id.Device() != nil || id.QP() != nil || id.Port() != 0 || id.LocalAddr().IsValid() || id.RemoteAddr().IsValid() {
		t.Error("stub ID accessors are not zero")
	}
	if ln.Addr().IsValid() {
		t.Errorf("stub Listener.Addr() = %v, want the zero value", ln.Addr())
	}
	if err := id.Close(); err != nil {
		t.Errorf("ID.Close() = %v, want nil", err)
	}
	if err := ln.Close(); err != nil {
		t.Errorf("Listener.Close() = %v, want nil", err)
	}
}
