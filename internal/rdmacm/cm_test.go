package rdmacm_test

import (
	"context"
	"errors"
	"fmt"
	"net/netip"
	"syscall"
	"testing"

	"github.com/wzshiming/rdma/internal/rdmacm"
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
	tooLong := func(n int) *rdmacm.ConnParam { return &rdmacm.ConnParam{PrivateData: make([]byte, n)} }
	tests := []struct {
		name string
		call func() error
	}{
		{"Resolve/zero remote", func() error { _, err := rdmacm.Resolve(ctx, netip.AddrPort{}, netip.AddrPort{}); return err }},
		{"Resolve/remote without address", func() error {
			_, err := rdmacm.Resolve(ctx, netip.AddrPort{}, netip.AddrPortFrom(netip.Addr{}, 4791))
			return err
		}},
		{"Resolve/remote port 0", func() error {
			_, err := rdmacm.Resolve(ctx, netip.AddrPort{}, netip.AddrPortFrom(v4.Addr(), 0))
			return err
		}},
		{"Resolve/local without address", func() error {
			_, err := rdmacm.Resolve(ctx, netip.AddrPortFrom(netip.Addr{}, 1), v4)
			return err
		}},
		{"Resolve/mixed families", func() error { _, err := rdmacm.Resolve(ctx, v6, v4); return err }},
		{"Listen/zero address", func() error { _, err := rdmacm.Listen(netip.AddrPort{}, 1); return err }},
		{"Listen/port without address", func() error { _, err := rdmacm.Listen(netip.AddrPortFrom(netip.Addr{}, 4791), 1); return err }},
		{"Dialer/zero remote", func() error { _, err := rdmacm.Dialer{}.Resolve(ctx, netip.AddrPort{}, netip.AddrPort{}); return err }},
		{"Dialer/ACK timeout above 31", func() error {
			_, err := rdmacm.Dialer{AckTimeout: new(uint8(32))}.Resolve(ctx, netip.AddrPort{}, v4)
			return err
		}},
		{"ListenConfig/zero address", func() error { _, err := rdmacm.ListenConfig{}.Listen(netip.AddrPort{}, 1); return err }},
		{"ListenConfig/ACK timeout above 31", func() error {
			_, err := rdmacm.ListenConfig{AckTimeout: new(uint8(32))}.Listen(netip.AddrPortFrom(v4.Addr(), 0), 1)
			return err
		}},
		{"Connect/private data too long", func() error {
			_, err := new(rdmacm.ID).Connect(ctx, tooLong(rdmacm.MaxConnectPrivateData+1))
			return err
		}},
		{"Request.Accept/private data too long", func() error {
			return new(rdmacm.Request).Accept(ctx, tooLong(rdmacm.MaxAcceptPrivateData+1))
		}},
		{"Request.Reject/private data too long", func() error {
			return new(rdmacm.Request).Reject(make([]byte, rdmacm.MaxRejectPrivateData+1))
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
	if rdmacm.MaxConnectPrivateData != 56 || rdmacm.MaxAcceptPrivateData != 196 || rdmacm.MaxRejectPrivateData != 148 {
		t.Errorf("limits = %d/%d/%d, want 56/196/148", rdmacm.MaxConnectPrivateData, rdmacm.MaxAcceptPrivateData, rdmacm.MaxRejectPrivateData)
	}
}

func TestEventString(t *testing.T) {
	tests := []struct {
		v    fmt.Stringer
		want string
	}{
		{rdmacm.EventAddrResolved, "ADDR_RESOLVED"},
		{rdmacm.EventAddrError, "ADDR_ERROR"},
		{rdmacm.EventRouteResolved, "ROUTE_RESOLVED"},
		{rdmacm.EventRouteError, "ROUTE_ERROR"},
		{rdmacm.EventConnectRequest, "CONNECT_REQUEST"},
		{rdmacm.EventConnectResponse, "CONNECT_RESPONSE"},
		{rdmacm.EventConnectError, "CONNECT_ERROR"},
		{rdmacm.EventUnreachable, "UNREACHABLE"},
		{rdmacm.EventRejected, "REJECTED"},
		{rdmacm.EventEstablished, "ESTABLISHED"},
		{rdmacm.EventDisconnected, "DISCONNECTED"},
		{rdmacm.EventDeviceRemoval, "DEVICE_REMOVAL"},
		{rdmacm.EventMulticastJoin, "MULTICAST_JOIN"},
		{rdmacm.EventMulticastError, "MULTICAST_ERROR"},
		{rdmacm.EventAddrChange, "ADDR_CHANGE"},
		{rdmacm.EventTimewaitExit, "TIMEWAIT_EXIT"},
		{rdmacm.Event(16), "Event(16)"},
	}
	for _, tt := range tests {
		if got := tt.v.String(); got != tt.want {
			t.Errorf("%T(%d).String() = %q, want %q", tt.v, tt.v, got, tt.want)
		}
	}
}
