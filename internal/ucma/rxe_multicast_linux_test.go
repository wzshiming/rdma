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

	"github.com/wzshiming/rdma/internal/rdmatest"
)

// A datagram identifier bound to the device's address joins an IPv4 group:
// MULTICAST_JOIN names the membership and carries the group's GID, and the
// leave counts the event.
func TestMulticastOnTheDevice(t *testing.T) {
	dev := rdmatest.Device(t)
	linkLayer, _ := os.ReadFile(filepath.Join("/sys/class/infiniband", dev.Name, "ports", strconv.Itoa(dev.Port), "link_layer"))
	if dev.GIDIndex < 0 || strings.TrimSpace(string(linkLayer)) != "Ethernet" {
		t.Skipf("%s is not RoCE, whose group GIDs the test knows", dev.Name)
	}
	const uid, mcUID = 0x6006, 0x7007
	c := openChannel(t)
	id, err := c.CreateID(uid, PortSpaceUDP, QPTypeUD)
	if err != nil {
		t.Fatalf("CreateID(UDP) error: %v", err)
	}
	if err := c.Bind(id, netip.AddrPortFrom(dev.IP, 0)); err != nil {
		t.Fatalf("Bind(%v:0) error: %v", dev.IP, err)
	}
	mcID, err := c.JoinMulticast(id, mcUID, testGroup, JoinFullMember)
	if err != nil {
		t.Fatalf("JoinMulticast(%v) error: %v", testGroup, err)
	}
	ev := nextEvent(t, c)
	if ev.Type != EventMulticastJoin || ev.Status != 0 || ev.UID != mcUID || ev.ID != mcID {
		t.Fatalf("after JoinMulticast: %v status %d uid %#x id %d; want MULTICAST_JOIN, status 0, uid %#x, id %d", ev.Type, ev.Status, ev.UID, ev.ID, mcUID, mcID)
	}
	ud := ev.UD()
	ah := ud.AHAttr
	t.Logf("MULTICAST_JOIN of membership %d: qp_num %#x, qkey %#x, %d bytes of private data; dgid %v, sgid_index %d (the address's %d), hop_limit %d, traffic_class %d, flow_label %#x, dlid %#x, sl %d, static_rate %d, is_global %d, port %d",
		mcID, ud.QPNum, ud.QKey, len(ud.PrivateData), netip.AddrFrom16(ah.Grh.Dgid), ah.Grh.Sgid_index, dev.GIDIndex, ah.Grh.Hop_limit, ah.Grh.Traffic_class, ah.Grh.Flow_label, ah.Dlid, ah.Sl, ah.Static_rate, ah.Is_global, ah.Port_num)
	v2 := testGroup.As16() // RoCE v2: the v4-mapped address
	v1 := v2
	v1[0], v1[1] = 0xff, 0x0e // RoCE v1: the same under ff0e::
	if ah.Grh.Dgid != v2 && ah.Grh.Dgid != v1 {
		t.Errorf("dgid %v, want the group's GID, %v on RoCE v2 or %v on RoCE v1", netip.AddrFrom16(ah.Grh.Dgid), netip.AddrFrom16(v2), netip.AddrFrom16(v1))
	}
	if ah.Is_global != 1 || ah.Port_num != uint8(dev.Port) || ud.QPNum != 0xffffff {
		t.Errorf("is_global %d, port %d, qp_num %#x; want 1, %d, 0xffffff", ah.Is_global, ah.Port_num, ud.QPNum, dev.Port)
	}
	n, err := c.LeaveMulticast(mcID)
	t.Logf("LeaveMulticast(%d): %d events reported, error %v", mcID, n, err)
	if n != 1 || err != nil {
		t.Errorf("LeaveMulticast() = %d, %v; want 1", n, err)
	}
}

// The kernel refuses a join from an identifier without a device and from
// one of the RC type.
func TestMulticastJoinsTheKernelRefuses(t *testing.T) {
	dev := rdmatest.Device(t)
	c := openChannel(t)
	unbound, err := c.CreateID(0x6006, PortSpaceUDP, QPTypeUD)
	if err != nil {
		t.Fatalf("CreateID(UDP) error: %v", err)
	}
	_, err = c.JoinMulticast(unbound, 0x7007, testGroup, JoinFullMember)
	t.Logf("JoinMulticast from an identifier without a device: %v (%#v)", err, err)
	if err != syscall.EINVAL {
		t.Errorf("JoinMulticast from an identifier without a device: error %#v, want syscall.EINVAL", err)
	}
	rc, _ := bindListener(t, c, dev, 0x6006)
	_, err = c.JoinMulticast(rc, 0x7007, testGroup, JoinFullMember)
	t.Logf("JoinMulticast from an RC identifier bound to %v: %v (%#v)", dev.IP, err, err)
	if err != syscall.EINVAL {
		t.Errorf("JoinMulticast from an RC identifier: error %#v, want syscall.EINVAL", err)
	}
}
