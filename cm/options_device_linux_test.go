//go:build linux && !android && (amd64 || arm64 || loong64 || ppc64le || riscv64 || s390x)

package cm_test

import (
	"context"
	"net/netip"
	"os"
	"strings"
	"syscall"
	"testing"

	"github.com/wzshiming/rdma/cm"
	"github.com/wzshiming/rdma/internal/rdmatest"
	"github.com/wzshiming/rdma/verbs"
)

// pairWith connects a client of im with the options of d to a listener of im
// with those of lc, both on the rdmatest device's IP.
func pairWith(t *testing.T, im impl, dev rdmatest.Dev, d cm.Dialer, lc cm.ListenConfig) (client conn, server *request) {
	t.Helper()
	ln := listenWith(t, im, lc, netip.AddrPortFrom(dev.IP, 0))
	client = resolveWith(t, im, d, netip.AddrPortFrom(dev.IP, 0), ln.Addr())
	res := connectAsync(client, nil)
	server = acceptRequest(t, ln)
	createQP(t, server.ID)
	ctx, cancel := context.WithTimeout(context.Background(), timeout)
	defer cancel()
	if err := server.Accept(ctx, nil); err != nil {
		t.Fatalf("Request.Accept() error: %v", err)
	}
	if r := <-res; r.err != nil {
		t.Fatalf("Connect() error: %v", r.err)
	}
	return client, server
}

// qpOptions are the attributes of a queue pair the options set.
type qpOptions struct {
	Timeout      uint8
	TrafficClass uint8
	SL           uint8
}

func qpOptionsOf(t *testing.T, id conn) qpOptions {
	t.Helper()
	a, err := id.QP().Query(verbs.QPAttrTimeout | verbs.QPAttrAV)
	if err != nil {
		t.Fatalf("QP.Query() error: %v", err)
	}
	return qpOptions{a.Timeout, a.AH.GRH.TrafficClass, a.AH.SL}
}

// TestOptionsReachTheQueuePair connects with the TOS and the ACK timeout on
// both ends, unset, set, and at the ends of their ranges, and reads the
// queue pairs. On RoCE the client's queue pair has the Dialer's ACK timeout
// and its TOS as the traffic class; the server's has the ListenConfig's ACK
// timeout, which the listener sets on the request's identifier, and the
// traffic class of the request rather than the ListenConfig's TOS; the
// service level is what the network device's priority mapping makes of the
// TOS, the same on both ends. An unset ACK timeout is the one the
// connection manager derives from the path, never the 0 a set 0 gives, and
// an unset TOS the device's default, 0 unless configured. On iWARP the
// options are taken and the connection made, the TOS going to the iWARP
// identifier, but the queue pair has neither an ACK timer nor an address
// handle: siw reports neither, which leaves the kernel's copy of them
// meaningless, so they are only logged.
func TestOptionsReachTheQueuePair(t *testing.T) {
	dev := testDevice(t)
	for _, tt := range []struct {
		name         string
		dialer       cm.Dialer
		listenConfig cm.ListenConfig
	}{
		{"unset", cm.Dialer{}, cm.ListenConfig{}},
		{"set", cm.Dialer{TOS: new(uint8(0x60)), AckTimeout: new(uint8(14))},
			cm.ListenConfig{TOS: new(uint8(0x20)), AckTimeout: new(uint8(16))}},
		{"the ends of the ranges", cm.Dialer{TOS: new(uint8(0xff)), AckTimeout: new(uint8(0))},
			cm.ListenConfig{TOS: new(uint8(0)), AckTimeout: new(uint8(31))}},
	} {
		t.Run(tt.name, func(t *testing.T) {
			forEachImpl(t, func(t *testing.T, im impl) {
				client, server := pairWith(t, im, dev, tt.dialer, tt.listenConfig)
				c, s := qpOptionsOf(t, client), qpOptionsOf(t, server.ID)
				t.Logf("client %+v, server %+v", c, s)
				if onIWARP(client) {
					return
				}
				checkAckTimeout(t, "client", c.Timeout, tt.dialer.AckTimeout)
				checkAckTimeout(t, "server", s.Timeout, tt.listenConfig.AckTimeout)
				var tos uint8
				if tt.dialer.TOS != nil {
					tos = *tt.dialer.TOS
				}
				if c.TrafficClass != tos || s.TrafficClass != tos {
					t.Errorf("traffic class %#x on the client, %#x on the server; want the client's TOS %#x on both", c.TrafficClass, s.TrafficClass, tos)
				}
				if c.SL != s.SL {
					t.Errorf("service level %d on the client, %d on the server; want the same", c.SL, s.SL)
				}
			})
		})
	}
}

// checkAckTimeout checks the ACK timeout got of a queue pair against the
// option set, nil for unset.
func checkAckTimeout(t *testing.T, end string, got uint8, set *uint8) {
	t.Helper()
	switch {
	case set == nil && got == 0:
		t.Errorf("%s ACK timeout 0 with the option unset, want the one derived from the path", end)
	case set != nil && got != *set:
		t.Errorf("%s ACK timeout %d, want the option's %d", end, got, *set)
	}
}

// TestReuseAddrSharesALocalPort binds identifiers to one local address and
// port: two Dialers with ReuseAddr resolve from it, where one without
// ReuseAddr fails to bind; a ListenConfig with ReuseAddr binds to it too
// but fails to listen, as listening needs the port alone, while one without
// fails to bind.
func TestReuseAddrSharesALocalPort(t *testing.T) {
	dev := testDevice(t)
	forEachImpl(t, func(t *testing.T, im impl) {
		ln := listenOn(t, im, netip.AddrPortFrom(dev.IP, 0))
		local := freeEndpoints(t, dev).client
		reuse := cm.Dialer{ReuseAddr: true}
		resolveWith(t, im, reuse, local, ln.Addr())
		resolveWith(t, im, reuse, local, ln.Addr())
		ctx, cancel := context.WithTimeout(context.Background(), timeout)
		defer cancel()
		if id, err := im.resolveWith(ctx, cm.Dialer{}, local, ln.Addr()); !isOpError(err, "rdma_resolve_addr", syscall.EADDRINUSE) {
			closeIfOpen(id)
			t.Errorf("Resolve(%v) without ReuseAddr: error %v, want EADDRINUSE from rdma_resolve_addr", local, err)
		}
		for _, tt := range []struct {
			config cm.ListenConfig
			op     string
		}{
			{cm.ListenConfig{ReuseAddr: true}, "rdma_listen"},
			{cm.ListenConfig{}, "rdma_bind_addr"},
		} {
			if l, err := im.listenWith(tt.config, local, 1); !isOpError(err, tt.op, syscall.EADDRINUSE) {
				closeIfOpen(l)
				t.Errorf("%+v.Listen(%v): error %v, want EADDRINUSE from %s", tt.config, local, err, tt.op)
			}
		}
	})
}

// TestAFOnlyKeepsAWildcardPortToItsFamily listens on the IPv6 wildcard
// address on the port of an IPv4 listener: with AFOnly the two share the
// port, without it the IPv6 wildcard takes IPv4 as well and fails to bind,
// and unset the sysctl net.ipv6.bindv6only decides.
func TestAFOnlyKeepsAWildcardPortToItsFamily(t *testing.T) {
	dev := testDevice(t)
	sysctl, err := os.ReadFile("/proc/sys/net/ipv6/bindv6only")
	if err != nil {
		t.Skipf("no IPv6: %v", err)
	}
	bindv6only := strings.TrimSpace(string(sysctl)) == "1"
	t.Logf("net.ipv6.bindv6only = %s", strings.TrimSpace(string(sysctl)))
	forEachImpl(t, func(t *testing.T, im impl) {
		v4 := listenOn(t, im, netip.AddrPortFrom(dev.IP, 0))
		v6 := netip.AddrPortFrom(netip.IPv6Unspecified(), v4.Addr().Port())
		for _, tt := range []struct {
			name   string
			afOnly *bool
			shared bool
		}{
			{"true", new(true), true},
			{"false", new(false), false},
			{"unset", nil, bindv6only},
		} {
			ln, err := im.listenWith(cm.ListenConfig{AFOnly: tt.afOnly}, v6, 1)
			switch {
			case tt.shared && err != nil:
				t.Errorf("AFOnly %s: Listen(%v) error %v, want the port shared with %v", tt.name, v6, err, v4.Addr())
			case !tt.shared && !isOpError(err, "rdma_bind_addr", syscall.EADDRNOTAVAIL):
				t.Errorf("AFOnly %s: Listen(%v) error %v, want EADDRNOTAVAIL from rdma_bind_addr", tt.name, v6, err)
			}
			closeIfOpen(ln)
		}
	})
}

// closeIfOpen closes c, an ID or a Listener a call returned, if it is one.
func closeIfOpen(c interface{ Close() error }) {
	if c != nil {
		c.Close()
	}
}
