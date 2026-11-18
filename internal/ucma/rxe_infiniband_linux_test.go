//go:build linux && !android && (amd64 || arm64 || loong64 || ppc64le || riscv64 || s390x)

package ucma

import (
	"net/netip"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"syscall"
	"testing"
	"time"

	"github.com/wzshiming/rdma/internal/rdmatest"
)

// linkLayer reads the link layer of the device's port: InfiniBand, or
// Ethernet for RoCE.
func linkLayer(t *testing.T, dev rdmatest.Dev) string {
	t.Helper()
	b, err := os.ReadFile(filepath.Join("/sys/class/infiniband", dev.Name, "ports", strconv.Itoa(dev.Port), "link_layer"))
	if err != nil {
		t.Fatal(err)
	}
	return strings.TrimSpace(string(b))
}

// deviceGID is the GID the kernel gives the device's address: on RoCE the
// v4-mapped form of the IP address, on InfiniBand the port's GID, the first
// of its table.
func deviceGID(t *testing.T, dev rdmatest.Dev, link string) netip.Addr {
	t.Helper()
	if link != "InfiniBand" {
		return netip.AddrFrom16(dev.IP.As16())
	}
	b, err := os.ReadFile(filepath.Join("/sys/class/infiniband", dev.Name, "ports", strconv.Itoa(dev.Port), "gids", "0"))
	if err != nil {
		t.Fatal(err)
	}
	gid, err := netip.ParseAddr(strings.TrimSpace(string(b)))
	if err != nil {
		t.Fatal(err)
	}
	return gid
}

// tcpServiceID is the service ID of a port of PortSpaceTCP.
func tcpServiceID(port uint16) uint64 { return uint64(PortSpaceTCP)<<16 | uint64(port) }

// librdmacm's probe binds no device: the kernel takes AF_IB addresses
// whatever its devices are.
func TestProbeAFIBOnTheDevice(t *testing.T) {
	rdmatest.Device(t)
	ok := openChannel(t).ProbeAFIB()
	t.Logf("ProbeAFIB() = %v", ok)
	if !ok {
		t.Error("ProbeAFIB() = false; the kernel's rdma_cm has taken AF_IB addresses since Linux 3.11")
	}
}

// The GID query of an identifier of IP resolved to the device's address:
// the kernel makes InfiniBand addresses of the route's GIDs, the partition
// key QueryAddr reports and the service IDs of the TCP port space and the
// two ports.
func TestQueryGIDOnTheDevice(t *testing.T) {
	dev := rdmatest.Device(t)
	link := linkLayer(t, dev)
	lch := openChannel(t)
	_, bound := bindListener(t, lch, dev, 0x6001)
	c := openChannel(t)
	id := createID(t, c, 0x6002)
	resolve(t, c, dev, id, 0x6002, bound.Src)
	addrs, err := c.QueryAddr(id)
	if err != nil {
		t.Fatalf("QueryAddr() error: %v", err)
	}
	gids, err := c.QueryGID(id)
	if err != nil {
		t.Fatalf("QueryGID() error: %v", err)
	}
	t.Logf("%s (%s): QueryGID of %v -> %v: source %+v, destination %+v; device GUID %#x, port %d, pkey %#x, index %d",
		dev.Name, link, addrs.Src, addrs.Dst, gids.SrcIB, gids.DstIB, gids.NodeGUID, gids.PortNum, gids.PKey, gids.DevIndex)
	gid := deviceGID(t, dev, link)
	want := AddrInfo{
		SrcIB:    IBAddr{GID: gid, PKey: addrs.PKey, ServiceID: tcpServiceID(addrs.Src.Port())},
		DstIB:    IBAddr{GID: gid, PKey: addrs.PKey, ServiceID: tcpServiceID(bound.Src.Port())},
		NodeGUID: addrs.NodeGUID,
		PortNum:  addrs.PortNum,
		PKey:     addrs.PKey,
		DevIndex: addrs.DevIndex,
	}
	if gids != want {
		t.Errorf("QueryGID() = %+v\nwant %+v", gids, want)
	}
}

// The path query of an identifier of IP: no record while only its address
// is resolved, then one, the primary path between the GIDs QueryGID
// reports.
func TestQueryPathOnTheDevice(t *testing.T) {
	dev := rdmatest.Device(t)
	link := linkLayer(t, dev)
	lch := openChannel(t)
	_, bound := bindListener(t, lch, dev, 0x6101)
	c := openChannel(t)
	id := createID(t, c, 0x6102)
	if err := c.ResolveAddr(id, netip.AddrPortFrom(dev.IP, 0), bound.Src, 2*time.Second); err != nil {
		t.Fatalf("ResolveAddr() error: %v", err)
	}
	if ev := nextEvent(t, c); ev.Type != EventAddrResolved || ev.Status != 0 {
		t.Fatalf("after ResolveAddr: %v status %d, want ADDR_RESOLVED", ev.Type, ev.Status)
	}
	if paths, err := c.QueryPath(id); len(paths) != 0 || err != nil {
		t.Errorf("QueryPath() with the address resolved = %+v, %v; want no record", paths, err)
	}
	if err := c.ResolveRoute(id, 2*time.Second); err != nil {
		t.Fatalf("ResolveRoute() error: %v", err)
	}
	if ev := nextEvent(t, c); ev.Type != EventRouteResolved || ev.Status != 0 {
		t.Fatalf("after ResolveRoute: %v status %d, want ROUTE_RESOLVED", ev.Type, ev.Status)
	}
	paths, err := c.QueryPath(id)
	if err != nil {
		t.Fatalf("QueryPath() error: %v", err)
	}
	for i, p := range paths {
		t.Logf("%s (%s): record %d of the route: flags %#x, %+v", dev.Name, link, i, uint32(p.Flags), p.Path)
	}
	gids, err := c.QueryGID(id)
	if err != nil {
		t.Fatalf("QueryGID() error: %v", err)
	}
	if len(paths) != 1 {
		t.Fatalf("%d records with the route resolved, want 1", len(paths))
	}
	if p := paths[0]; p.Flags != PathGMP|PathPrimary|PathBidirectional || p.Path.SGID != gids.SrcIB.GID || p.Path.DGID != gids.DstIB.GID || !p.Path.Reversible {
		t.Errorf("record %+v; want the flags GMP, primary and bidirectional, the GIDs %v to %v of QueryGID, reversible", p, gids.SrcIB.GID, gids.DstIB.GID)
	}
}

// AF_IB on the device. The any address binds whatever the link layer, in
// the port space and with the port the kernel picks; the device's own GID
// binds and resolves on InfiniBand, where the route of the resolved address
// comes from the subnet administrator, and fails with ENODEV on RoCE,
// whose ports take no AF_IB address.
func TestAFIBOnTheDevice(t *testing.T) {
	dev := rdmatest.Device(t)
	link := linkLayer(t, dev)
	gid := deviceGID(t, dev, link)
	c := openChannel(t)

	anyID := createID(t, c, 0x6201)
	if err := c.BindIB(anyID, IBAddr{GID: netip.IPv6Unspecified(), ServiceID: tcpServiceID(0), ServiceIDMask: ipPortSpaceMask}); err != nil {
		t.Fatalf("BindIB(the any address in the TCP port space) error: %v", err)
	}
	bound, err := c.QueryAddr(anyID)
	if err != nil {
		t.Fatalf("QueryAddr() error: %v", err)
	}
	gids, err := c.QueryGID(anyID)
	t.Logf("bound to the any address: QueryAddr %+v; QueryGID %+v, %v", bound, gids, err)
	port := uint16(bound.SrcIB.ServiceID)
	if bound.Src.IsValid() || bound.SrcIB.GID != netip.IPv6Unspecified() || bound.SrcIB.ServiceID != tcpServiceID(port) || port == 0 || bound.SrcIB.ServiceIDMask != 1<<64-1 {
		t.Errorf("bound to %+v; want the any address with a port the kernel picked in the TCP port space, every bit of the service ID counting", bound.SrcIB)
	}
	if gids.SrcIB != bound.SrcIB || err != nil {
		t.Errorf("QueryGID() source %+v, %v; want the AF_IB address itself, %+v", gids.SrcIB, err, bound.SrcIB)
	}

	devID := createID(t, c, 0x6202)
	bindErr := c.BindIB(devID, IBAddr{GID: gid, PKey: 0xffff, ServiceID: tcpServiceID(0), ServiceIDMask: ipPortSpaceMask})
	t.Logf("%s (%s): BindIB to the GID %v: %v (%#v)", dev.Name, link, gid, bindErr, bindErr)
	resolveID := createID(t, c, 0x6203)
	dst := IBAddr{GID: gid, PKey: 0xffff, ServiceID: tcpServiceID(port), ServiceIDMask: 1<<64 - 1}
	resolveErr := c.ResolveIBAddr(resolveID, IBAddr{}, dst, 2*time.Second)
	t.Logf("%s (%s): ResolveIBAddr to %+v: %v (%#v)", dev.Name, link, dst, resolveErr, resolveErr)
	if link != "InfiniBand" {
		if bindErr != syscall.ENODEV || resolveErr != syscall.ENODEV {
			t.Errorf("BindIB and ResolveIBAddr to the GID %v of a RoCE port: errors %#v and %#v, want ENODEV", gid, bindErr, resolveErr)
		}
		return
	}

	if bindErr != nil {
		t.Errorf("BindIB to the GID %v error: %v", gid, bindErr)
	} else if info, err := c.QueryAddr(devID); info.SrcIB.GID != gid || info.NodeGUID != nodeGUID(t, dev.Name) || err != nil {
		t.Errorf("bound to the GID %v: QueryAddr() = %+v, %v; want that GID on %s", gid, info, err, dev.Name)
	}
	if resolveErr != nil {
		t.Fatalf("ResolveIBAddr() error: %v", resolveErr)
	}
	if ev := nextEvent(t, c); ev.Type != EventAddrResolved || ev.Status != 0 || ev.ID != resolveID {
		t.Fatalf("after ResolveIBAddr: %v status %d id %d, want ADDR_RESOLVED of %d", ev.Type, ev.Status, ev.ID, resolveID)
	}
	resolved, err := c.QueryAddr(resolveID)
	t.Logf("resolved: %+v, %v", resolved, err)
	if resolved.SrcIB.GID != gid || resolved.DstIB.GID != gid || err != nil {
		t.Errorf("QueryAddr() of the resolved identifier = %+v, %v; want %v at both ends", resolved, err, gid)
	}
	if err := c.ResolveRoute(resolveID, 2*time.Second); err != nil {
		t.Fatalf("ResolveRoute() error: %v", err)
	}
	if ev := nextEvent(t, c); ev.Type != EventRouteResolved || ev.Status != 0 {
		t.Fatalf("after ResolveRoute: %v status %d, want ROUTE_RESOLVED", ev.Type, ev.Status)
	}
	paths, err := c.QueryPath(resolveID)
	t.Logf("the route from the subnet administrator: %+v, %v", paths, err)
	if len(paths) != 1 || paths[0].Path.DGID != gid || err != nil {
		t.Errorf("QueryPath() = %+v, %v; want one record to %v", paths, err, gid)
	}
}

// The IB path option on the device. The record of a resolved route sets
// the route of another identifier whose address is resolved, in place of
// ResolveRoute: the kernel takes the first record flagged GMP, primary and
// bidirectional, queues ROUTE_RESOLVED itself and reports the record back,
// and a connection request takes the route to the listener.
func TestIBPathOptionOnTheDevice(t *testing.T) {
	dev := rdmatest.Device(t)
	link := linkLayer(t, dev)
	lch := openChannel(t)
	lid, bound := bindListener(t, lch, dev, 0x6301)
	if err := lch.Listen(lid, 1); err != nil {
		t.Fatalf("Listen() error: %v", err)
	}
	rch := openChannel(t)
	ref := createID(t, rch, 0x6302)
	resolve(t, rch, dev, ref, 0x6302, bound.Src)
	paths, err := rch.QueryPath(ref)
	if len(paths) != 1 || err != nil {
		t.Fatalf("QueryPath() of a resolved route = %+v, %v; want one record", paths, err)
	}
	t.Logf("%s (%s): the record to set: flags %#x, %+v", dev.Name, link, uint32(paths[0].Flags), paths[0].Path)

	c := openChannel(t)
	const uid = 0x6303
	id := createID(t, c, uid)
	err = c.SetIBPath(id, paths)
	t.Logf("SetIBPath() before the address is resolved: %v (%#v)", err, err)
	if err != syscall.EINVAL {
		t.Errorf("SetIBPath() before the address is resolved: error %#v, want EINVAL", err)
	}
	if err := c.ResolveAddr(id, netip.AddrPortFrom(dev.IP, 0), bound.Src, 2*time.Second); err != nil {
		t.Fatalf("ResolveAddr() error: %v", err)
	}
	if ev := nextEvent(t, c); ev.Type != EventAddrResolved || ev.Status != 0 {
		t.Fatalf("after ResolveAddr: %v status %d, want ADDR_RESOLVED", ev.Type, ev.Status)
	}
	alternate := paths[0]
	alternate.Flags = PathGMP | PathAlternate | PathBidirectional
	for _, tt := range []struct {
		name  string
		paths []PathData
	}{{"no record", nil}, {"an alternate record only", []PathData{alternate}}} {
		err := c.SetIBPath(id, tt.paths)
		t.Logf("SetIBPath() with %s: %v (%#v)", tt.name, err, err)
		if err != syscall.EINVAL {
			t.Errorf("SetIBPath() with %s: error %#v, want EINVAL", tt.name, err)
		}
	}
	if err := c.SetIBPath(id, []PathData{alternate, paths[0]}); err != nil {
		t.Fatalf("SetIBPath() error: %v", err)
	}
	if ev := nextEvent(t, c); ev.Type != EventRouteResolved || ev.Status != 0 || ev.UID != uid || ev.ID != id {
		t.Fatalf("after SetIBPath: %v status %d uid %#x id %d; want ROUTE_RESOLVED, status 0, uid %#x, id %d", ev.Type, ev.Status, ev.UID, ev.ID, uint64(uid), id)
	}
	got, err := c.QueryPath(id)
	t.Logf("QueryPath() after SetIBPath: %+v, %v", got, err)
	if len(got) != 1 || err != nil {
		t.Fatalf("QueryPath() after SetIBPath = %+v, %v; want the record set", got, err)
	}
	if got[0] != paths[0] {
		t.Errorf("QueryPath() after SetIBPath = %+v\nwant the record set, %+v", got[0], paths[0])
	}

	param := ConnParam{PrivateData: []byte("ucma path"), QPNum: 0xabcde, ResponderResources: 1, InitiatorDepth: 1, RetryCount: 7, RNRRetryCount: 7}
	if err := c.Connect(id, param, ECE{}); err != nil {
		t.Fatalf("Connect() error: %v", err)
	}
	req := nextEvent(t, lch)
	if req.Type != EventConnectRequest || req.Status != 0 {
		t.Fatalf("listener got %v status %d, want CONNECT_REQUEST", req.Type, req.Status)
	}
	if err := lch.Reject(req.ID, 0, nil); err != nil {
		t.Fatalf("Reject() error: %v", err)
	}
	if ev := nextEvent(t, c); ev.Type != EventRejected || ev.ID != id {
		t.Errorf("after Reject: %v status %d id %d, want REJECTED of %d", ev.Type, ev.Status, ev.ID, id)
	}
}
