// Package rdmatest finds an RDMA device for integration tests.
package rdmatest

import (
	"net"
	"net/netip"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"testing"
)

// Dev is an RDMA device port usable by integration tests.
type Dev struct {
	Name     string     // RDMA device, e.g. "rxe0"
	Port     int        // port number, starting at 1
	Netdev   string     // network interface of the port, e.g. "eth0"
	IP       netip.Addr // IPv4 address of Netdev
	GIDIndex int        // RoCE v2 GID index of IP; 0 on InfiniBand, -1 on iWARP
}

// Device returns the first device under /sys/class/infiniband with an ACTIVE
// port whose network interface has an IPv4 address. Without one it skips t,
// or fails t when the environment sets RDMA_TEST_REQUIRE=1.
func Device(t testing.TB) Dev {
	t.Helper()
	return device(t, "/sys/class/infiniband", interfaceAddrs)
}

func device(t testing.TB, root string, addrs func(netdev string) ([]netip.Addr, error)) Dev {
	t.Helper()
	if d, ok := find(root, addrs); ok {
		return d
	}
	msg := "rdmatest: no RDMA device with an ACTIVE port and an IPv4 address under " + root
	if os.Getenv("RDMA_TEST_REQUIRE") == "1" {
		t.Fatal(msg + " (RDMA_TEST_REQUIRE=1)")
	} else {
		t.Skip(msg)
	}
	return Dev{}
}

func find(root string, addrs func(netdev string) ([]netip.Addr, error)) (Dev, bool) {
	devs, _ := os.ReadDir(root)
	for _, d := range devs {
		dir := filepath.Join(root, d.Name())
		ports, _ := os.ReadDir(filepath.Join(dir, "ports"))
		for _, p := range ports {
			portDir := filepath.Join(dir, "ports", p.Name())
			if readFile(portDir, "state") != "4: ACTIVE" {
				continue
			}
			for _, netdev := range netdevs(dir) {
				ips, _ := addrs(netdev)
				for _, ip := range ips {
					if !ip.Is4() {
						continue
					}
					if i, ok := gidIndex(dir, portDir, ip); ok {
						port, _ := strconv.Atoi(p.Name())
						return Dev{Name: d.Name(), Port: port, Netdev: netdev, IP: ip, GIDIndex: i}, true
					}
				}
			}
		}
	}
	return Dev{}, false
}

// netdevs returns the network interfaces of an RDMA device: software devices
// (rxe, siw) name theirs in parent, hardware ones list them under device/net.
func netdevs(dir string) []string {
	if parent := readFile(dir, "parent"); parent != "" {
		return []string{parent}
	}
	entries, _ := os.ReadDir(filepath.Join(dir, "device", "net"))
	names := make([]string, 0, len(entries))
	for _, e := range entries {
		names = append(names, e.Name())
	}
	return names
}

func gidIndex(dir, portDir string, ip netip.Addr) (int, bool) {
	switch {
	case readFile(dir, "node_type") == "4: RNIC":
		return -1, true // iWARP addresses peers by IP, not GID
	case readFile(portDir, "link_layer") == "InfiniBand":
		return 0, true
	}
	return roceV2GID(portDir, ip)
}

// roceV2GID returns the index of the RoCE v2 GID holding ip.
func roceV2GID(portDir string, ip netip.Addr) (int, bool) {
	gids, _ := os.ReadDir(filepath.Join(portDir, "gids"))
	for _, g := range gids {
		// Unpopulated entries fail to read their type.
		if readFile(portDir, "gid_attrs", "types", g.Name()) != "RoCE v2" {
			continue
		}
		if gid, err := netip.ParseAddr(readFile(portDir, "gids", g.Name())); err == nil && gid.Unmap() == ip {
			i, _ := strconv.Atoi(g.Name())
			return i, true
		}
	}
	return 0, false
}

// readFile returns the trimmed content of a sysfs attribute, or "" if it cannot be read.
func readFile(elem ...string) string {
	b, _ := os.ReadFile(filepath.Join(elem...))
	return strings.TrimSpace(string(b))
}

func interfaceAddrs(name string) ([]netip.Addr, error) {
	ifi, err := net.InterfaceByName(name)
	if err != nil {
		return nil, err
	}
	addrs, err := ifi.Addrs()
	if err != nil {
		return nil, err
	}
	var ips []netip.Addr
	for _, a := range addrs {
		if n, ok := a.(*net.IPNet); ok {
			if ip, ok := netip.AddrFromSlice(n.IP); ok {
				ips = append(ips, ip.Unmap())
			}
		}
	}
	return ips, nil
}
