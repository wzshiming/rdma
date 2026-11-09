package cm_test

import (
	"context"
	"errors"
	"fmt"
	"net/netip"
	"syscall"
	"testing"

	"github.com/wzshiming/rdma/cm"
	"github.com/wzshiming/rdma/verbs"
)

var (
	v4 = netip.MustParseAddrPort("10.200.0.1:4791")
	v6 = netip.MustParseAddrPort("[fd00::1]:4791")
)

// TestInvalidArgumentsFailBeforeTheCall checks the argument validation every
// platform performs before touching librdmacm.
func TestInvalidArgumentsFailBeforeTheCall(t *testing.T) {
	ctx := context.Background()
	tooLong := func(n int) *cm.ConnParam { return &cm.ConnParam{PrivateData: make([]byte, n)} }
	tests := []struct {
		name string
		call func() error
	}{
		{"Resolve/zero remote", func() error { _, err := cm.Resolve(ctx, netip.AddrPort{}, netip.AddrPort{}); return err }},
		{"Resolve/remote without address", func() error {
			_, err := cm.Resolve(ctx, netip.AddrPort{}, netip.AddrPortFrom(netip.Addr{}, 4791))
			return err
		}},
		{"Resolve/remote port 0", func() error {
			_, err := cm.Resolve(ctx, netip.AddrPort{}, netip.AddrPortFrom(v4.Addr(), 0))
			return err
		}},
		{"Resolve/local without address", func() error {
			_, err := cm.Resolve(ctx, netip.AddrPortFrom(netip.Addr{}, 1), v4)
			return err
		}},
		{"Resolve/mixed families", func() error { _, err := cm.Resolve(ctx, v6, v4); return err }},
		{"Listen/zero address", func() error { _, err := cm.Listen(netip.AddrPort{}, 1); return err }},
		{"Listen/port without address", func() error { _, err := cm.Listen(netip.AddrPortFrom(netip.Addr{}, 4791), 1); return err }},
		{"Connect/private data too long", func() error {
			_, err := new(cm.ID).Connect(ctx, tooLong(cm.MaxConnectPrivateData+1))
			return err
		}},
		{"Request.Accept/private data too long", func() error {
			return new(cm.Request).Accept(ctx, tooLong(cm.MaxAcceptPrivateData+1))
		}},
		{"Request.Reject/private data too long", func() error {
			return new(cm.Request).Reject(make([]byte, cm.MaxRejectPrivateData+1))
		}},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			err := tt.call()
			if !errors.Is(err, syscall.EINVAL) {
				t.Fatalf("err = %v, want syscall.EINVAL", err)
			}
			var opErr *verbs.OpError
			if !errors.As(err, &opErr) || opErr.Op == "" {
				t.Fatalf("err = %#v, want *verbs.OpError naming the rdma_cm function", err)
			}
			if errors.Is(err, errors.ErrUnsupported) {
				t.Fatalf("err = %v, the argument check must run before the platform stub", err)
			}
		})
	}
}

func TestPrivateDataLimits(t *testing.T) {
	if cm.MaxConnectPrivateData != 56 || cm.MaxAcceptPrivateData != 196 || cm.MaxRejectPrivateData != 148 {
		t.Errorf("limits = %d/%d/%d, want 56/196/148", cm.MaxConnectPrivateData, cm.MaxAcceptPrivateData, cm.MaxRejectPrivateData)
	}
}

func TestRejectedError(t *testing.T) {
	err := error(&cm.RejectedError{PrivateData: []byte("busy"), Reason: 28})
	if want := "cm: connection rejected by peer (reason 28)"; err.Error() != want {
		t.Errorf("Error() = %q, want %q", err.Error(), want)
	}
	if !errors.Is(err, syscall.ECONNREFUSED) {
		t.Error("errors.Is(err, syscall.ECONNREFUSED) = false, want true")
	}
	if errors.Is(err, syscall.ECONNRESET) {
		t.Error("errors.Is(err, syscall.ECONNRESET) = true, want false")
	}
	var rej *cm.RejectedError
	if !errors.As(err, &rej) || string(rej.PrivateData) != "busy" {
		t.Errorf("errors.As(err, *RejectedError) failed or data mismatch: %#v", err)
	}
}

func TestEventString(t *testing.T) {
	tests := []struct {
		v    fmt.Stringer
		want string
	}{
		{cm.EventAddrResolved, "ADDR_RESOLVED"},
		{cm.EventAddrError, "ADDR_ERROR"},
		{cm.EventRouteResolved, "ROUTE_RESOLVED"},
		{cm.EventRouteError, "ROUTE_ERROR"},
		{cm.EventConnectRequest, "CONNECT_REQUEST"},
		{cm.EventConnectResponse, "CONNECT_RESPONSE"},
		{cm.EventConnectError, "CONNECT_ERROR"},
		{cm.EventUnreachable, "UNREACHABLE"},
		{cm.EventRejected, "REJECTED"},
		{cm.EventEstablished, "ESTABLISHED"},
		{cm.EventDisconnected, "DISCONNECTED"},
		{cm.EventDeviceRemoval, "DEVICE_REMOVAL"},
		{cm.EventMulticastJoin, "MULTICAST_JOIN"},
		{cm.EventMulticastError, "MULTICAST_ERROR"},
		{cm.EventAddrChange, "ADDR_CHANGE"},
		{cm.EventTimewaitExit, "TIMEWAIT_EXIT"},
		{cm.Event(16), "Event(16)"},
	}
	for _, tt := range tests {
		if got := tt.v.String(); got != tt.want {
			t.Errorf("%T(%d).String() = %q, want %q", tt.v, tt.v, got, tt.want)
		}
	}
}
