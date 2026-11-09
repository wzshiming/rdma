package rdmatest

import (
	"net"
	"net/netip"
	"slices"
	"testing"
)

// recordTB records Skip and Fatal instead of stopping the test.
type recordTB struct {
	testing.TB
	skipped, failed bool
}

func (r *recordTB) Helper()           {}
func (r *recordTB) Skip(args ...any)  { r.skipped = true }
func (r *recordTB) Fatal(args ...any) { r.failed = true }

func noAddrs(string) ([]netip.Addr, error) { return nil, nil }

func TestDevice(t *testing.T) {
	d := Device(t)
	t.Logf("%+v", d)
	if d.Name == "" || d.Port < 1 || d.Netdev == "" || !d.IP.Is4() || d.GIDIndex < -1 {
		t.Errorf("Device() = %+v, want a named port with an IPv4 address", d)
	}
}

func TestDeviceFromSysfs(t *testing.T) {
	ip := netip.MustParseAddr("10.0.0.1")
	for _, tt := range []struct {
		name  string
		root  string
		addrs map[string][]netip.Addr
		want  Dev // the zero Dev means none is found
	}{
		{
			name:  "RoCE device",
			root:  "testdata/roce",
			addrs: map[string][]netip.Addr{"eth0": {ip}},
			want:  Dev{Name: "rxe0", Port: 1, Netdev: "eth0", IP: ip, GIDIndex: 4},
		},
		{
			name:  "no IPv4",
			root:  "testdata/roce",
			addrs: map[string][]netip.Addr{"eth0": {netip.MustParseAddr("fe80::215:5dff:fe01:203")}},
		},
		{
			name:  "no ACTIVE port",
			root:  "testdata/down",
			addrs: map[string][]netip.Addr{"eth0": {ip}},
		},
		{
			name:  "iWARP device",
			root:  "testdata/iwarp",
			addrs: map[string][]netip.Addr{"eth0": {ip}},
			want:  Dev{Name: "siw0", Port: 1, Netdev: "eth0", IP: ip, GIDIndex: -1},
		},
		{
			name:  "hardware RoCE device",
			root:  "testdata/hca",
			addrs: map[string][]netip.Addr{"ens1f0np0": {ip}},
			want:  Dev{Name: "mlx5_0", Port: 1, Netdev: "ens1f0np0", IP: ip, GIDIndex: 3},
		},
		{
			name:  "InfiniBand device",
			root:  "testdata/ib",
			addrs: map[string][]netip.Addr{"ib0": {ip}},
			want:  Dev{Name: "mlx5_0", Port: 1, Netdev: "ib0", IP: ip, GIDIndex: 0},
		},
	} {
		t.Run(tt.name, func(t *testing.T) {
			t.Setenv("RDMA_TEST_REQUIRE", "")
			tb := &recordTB{TB: t}
			got := device(tb, tt.root, func(netdev string) ([]netip.Addr, error) {
				return tt.addrs[netdev], nil
			})
			if got != tt.want || tb.skipped != (tt.want == Dev{}) {
				t.Errorf("device() = %+v (skipped=%v), want %+v", got, tb.skipped, tt.want)
			}
		})
	}
}

func TestDeviceWithoutDevice(t *testing.T) {
	for _, tt := range []struct {
		name     string
		require  string
		wantSkip bool
		wantFail bool
	}{
		{name: "skips by default", wantSkip: true},
		{name: "fails with RDMA_TEST_REQUIRE=1", require: "1", wantFail: true},
	} {
		t.Run(tt.name, func(t *testing.T) {
			t.Setenv("RDMA_TEST_REQUIRE", tt.require)
			tb := &recordTB{TB: t}
			device(tb, t.TempDir(), noAddrs)
			if tb.skipped != tt.wantSkip || tb.failed != tt.wantFail {
				t.Errorf("skipped=%v failed=%v, want skipped=%v failed=%v", tb.skipped, tb.failed, tt.wantSkip, tt.wantFail)
			}
		})
	}
}

func TestInterfaceAddrsUnmapsIPv4(t *testing.T) {
	ifaces, err := net.Interfaces()
	if err != nil {
		t.Fatal(err)
	}
	loopback := netip.MustParseAddr("127.0.0.1")
	for _, ifi := range ifaces {
		if ifi.Flags&net.FlagLoopback == 0 {
			continue
		}
		addrs, err := interfaceAddrs(ifi.Name)
		if err != nil {
			t.Fatal(err)
		}
		if !slices.Contains(addrs, loopback) {
			t.Errorf("interfaceAddrs(%q) = %v, want it to contain %v", ifi.Name, addrs, loopback)
		}
		return
	}
	t.Skip("no loopback interface")
}
