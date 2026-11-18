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
	did := new(cm.DatagramID)
	dln := new(cm.DatagramListener)
	tests := []struct {
		name string
		call func() error
	}{
		{"Resolve", func() error { _, err := cm.Resolve(ctx, netip.AddrPort{}, v4); return err }},
		{"Listen", func() error { _, err := cm.Listen(netip.AddrPortFrom(v4.Addr(), 0), 1); return err }},
		{"Dialer.Resolve", func() error {
			_, err := cm.Dialer{TOS: new(uint8(0x20)), AckTimeout: new(uint8(14))}.Resolve(ctx, netip.AddrPort{}, v4)
			return err
		}},
		{"ListenConfig.Listen", func() error {
			_, err := cm.ListenConfig{ReuseAddr: true, AFOnly: new(true)}.Listen(netip.AddrPortFrom(v4.Addr(), 0), 1)
			return err
		}},
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
		{"ResolveDatagram", func() error { _, err := cm.ResolveDatagram(ctx, netip.AddrPort{}, v4); return err }},
		{"Dialer.ResolveDatagram", func() error {
			_, err := cm.Dialer{TOS: new(uint8(0x20))}.ResolveDatagram(ctx, netip.AddrPort{}, v4)
			return err
		}},
		{"BindDatagram", func() error { _, err := cm.BindDatagram(netip.AddrPortFrom(v4.Addr(), 0)); return err }},
		{"ListenDatagram", func() error { _, err := cm.ListenDatagram(netip.AddrPortFrom(v4.Addr(), 0), 1); return err }},
		{"ListenConfig.ListenDatagram", func() error {
			_, err := cm.ListenConfig{ReuseAddr: true}.ListenDatagram(netip.AddrPortFrom(v4.Addr(), 0), 1)
			return err
		}},
		{"DatagramListener.Accept", func() error { _, err := dln.Accept(ctx); return err }},
		{"DatagramID.CreateQP", func() error {
			_, err := did.CreateQP(verbs.PDFromPointer(nil), &verbs.QPInitAttr{Type: verbs.QPTypeUD})
			return err
		}},
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
	if did.Device() != nil || did.QP() != nil || did.Port() != 0 || did.LocalAddr().IsValid() || did.RemoteAddr().IsValid() || dln.Addr().IsValid() {
		t.Error("stub DatagramID and DatagramListener accessors are not zero")
	}
	if err := did.Close(); err != nil {
		t.Errorf("DatagramID.Close() = %v, want nil", err)
	}
	if err := dln.Close(); err != nil {
		t.Errorf("DatagramListener.Close() = %v, want nil", err)
	}
	if err := id.Close(); err != nil {
		t.Errorf("ID.Close() = %v, want nil", err)
	}
	if err := ln.Close(); err != nil {
		t.Errorf("Listener.Close() = %v, want nil", err)
	}
}
