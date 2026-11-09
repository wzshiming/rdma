package rdma

import (
	"context"
	"errors"
	"net"
	"net/netip"
	"testing"
)

func TestAddrString(t *testing.T) {
	tests := []struct {
		name string
		addr *Addr
		want string
	}{
		{"nil", nil, "<nil>"},
		{"v4", &Addr{IP: net.IPv4(10, 200, 0, 1), Port: 4791}, "10.200.0.1:4791"},
		{"v6", &Addr{IP: net.ParseIP("fe80::1"), Port: 1, Zone: "eth0"}, "[fe80::1%eth0]:1"},
		{"wildcard", &Addr{Port: 80}, ":80"},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			if got := tt.addr.String(); got != tt.want {
				t.Errorf("String() = %q, want %q", got, tt.want)
			}
			if tt.addr.Network() != "rdma" {
				t.Errorf("Network() = %q, want %q", tt.addr.Network(), "rdma")
			}
		})
	}
}

func TestAddrRoundTripsThroughAddrPort(t *testing.T) {
	tests := []struct {
		in   netip.AddrPort
		want string
	}{
		{netip.MustParseAddrPort("10.200.0.1:4791"), "10.200.0.1:4791"},
		{netip.MustParseAddrPort("[::ffff:10.200.0.1]:4791"), "10.200.0.1:4791"},
		{netip.MustParseAddrPort("[fe80::1%eth0]:7"), "[fe80::1%eth0]:7"},
	}
	for _, tt := range tests {
		a := addrFromAddrPort(tt.in)
		if a.String() != tt.want {
			t.Errorf("addrFromAddrPort(%v) = %v, want %v", tt.in, a, tt.want)
		}
		if back := a.addrPort(); back != netip.AddrPortFrom(tt.in.Addr().Unmap(), tt.in.Port()) {
			t.Errorf("addrPort() = %v, want %v", back, tt.in)
		}
	}
	if a := addrFromAddrPort(netip.AddrPort{}); a != nil {
		t.Errorf("addrFromAddrPort(zero) = %v, want nil", a)
	}
}

func TestResolveAddr(t *testing.T) {
	tests := []struct {
		network, address string
		want             string
		wantErr          bool
	}{
		{"rdma", "10.200.0.1:4791", "10.200.0.1:4791", false},
		{"rdma4", "10.200.0.1:4791", "10.200.0.1:4791", false},
		{"rdma6", "[::1]:4791", "[::1]:4791", false},
		{"rdma", ":4791", ":4791", false},
		{"rdma", "localhost:4791", "", false},  // resolved through the default resolver
		{"rdma6", "10.200.0.1:4791", "", true}, // wrong family
		{"rdma4", "[::1]:4791", "", true},
		{"tcp", "10.200.0.1:4791", "", true}, // unknown network
		{"rdma", "10.200.0.1", "", true},     // missing port
		{"rdma", "10.200.0.1:nope", "", true},
		{"rdma", "", "", true},
	}
	for _, tt := range tests {
		t.Run(tt.network+" "+tt.address, func(t *testing.T) {
			got, err := ResolveAddr(tt.network, tt.address)
			if tt.wantErr {
				var opErr *net.OpError
				if err == nil || !errors.As(err, &opErr) || opErr.Net != "rdma" {
					t.Fatalf("ResolveAddr() = %v, %v; want a *net.OpError on net rdma", got, err)
				}
				return
			}
			if err != nil {
				t.Fatalf("ResolveAddr() error: %v", err)
			}
			if tt.want != "" && got.String() != tt.want {
				t.Errorf("ResolveAddr() = %v, want %v", got, tt.want)
			}
			if tt.want == "" && (got.IP == nil || got.Port != 4791) {
				t.Errorf("ResolveAddr() = %v, want a resolved IP with port 4791", got)
			}
		})
	}
}

func TestResolveAddrHonoursContext(t *testing.T) {
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	_, err := resolveAddr(ctx, "rdma", "no-such-host.invalid:1")
	if err == nil {
		t.Fatal("resolveAddr() with a cancelled context succeeded")
	}
}

func TestConfigDefaultsAndValidation(t *testing.T) {
	def := Config{}.withDefaults()
	if def != (Config{MessageSize: 16 << 10, ReceiveDepth: 32, SendDepth: 32}) {
		t.Errorf("zero Config defaults = %+v", def)
	}
	set := Config{MessageSize: 4096, ReceiveDepth: 1, SendDepth: 2, MaxInline: 64}
	if got := set.withDefaults(); got != set {
		t.Errorf("withDefaults() changed an explicit Config: %+v", got)
	}
	bad := []Config{
		{MessageSize: 255},
		{MessageSize: -1},
		{ReceiveDepth: -1},
		{SendDepth: -1},
		{MaxInline: -1},
	}
	for _, c := range bad {
		if err := c.withDefaults().validate(); err == nil {
			t.Errorf("validate(%+v) = nil, want an error", c)
		}
	}
	if err := def.validate(); err != nil {
		t.Errorf("validate(defaults) error: %v", err)
	}
}
