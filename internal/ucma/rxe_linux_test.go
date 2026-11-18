//go:build linux && !android && (amd64 || arm64 || loong64 || ppc64le || riscv64 || s390x)

package ucma

import (
	"bytes"
	"context"
	"encoding/json"
	"net/netip"
	"os"
	"os/exec"
	"path/filepath"
	"strconv"
	"strings"
	"sync"
	"syscall"
	"testing"
	"time"

	"github.com/wzshiming/rdma/internal/rdmatest"
)

const deviceTimeout = 10 * time.Second

// openChannel opens a channel of the device for the test.
func openChannel(t *testing.T) *Channel {
	t.Helper()
	c, err := Open()
	if err != nil {
		t.Fatalf("Open() error: %v", err)
	}
	t.Cleanup(func() { c.Close() })
	return c
}

// nextEvent fetches the next event of c and logs it.
func nextEvent(t *testing.T, c *Channel) Event {
	t.Helper()
	ctx, cancel := context.WithTimeout(context.Background(), deviceTimeout)
	defer cancel()
	ev, err := c.GetEvent(ctx)
	if err != nil {
		t.Fatalf("GetEvent() error: %v", err)
	}
	t.Logf("event %v, status %d, uid %#x, id %d", ev.Type, ev.Status, ev.UID, ev.ID)
	return ev
}

// createID creates an RC identifier of the TCP port space.
func createID(t *testing.T, c *Channel, uid uint64) uint32 {
	t.Helper()
	id, err := c.CreateID(uid, PortSpaceTCP, QPTypeRC)
	if err != nil {
		t.Fatalf("CreateID() error: %v", err)
	}
	return id
}

// bindListener binds a new identifier to port 0 of the device's address and
// returns it with what QueryAddr reports of it.
func bindListener(t *testing.T, c *Channel, dev rdmatest.Dev, uid uint64) (uint32, AddrInfo) {
	t.Helper()
	id := createID(t, c, uid)
	if err := c.Bind(id, netip.AddrPortFrom(dev.IP, 0)); err != nil {
		t.Fatalf("Bind(%v:0) error: %v", dev.IP, err)
	}
	info, err := c.QueryAddr(id)
	if err != nil {
		t.Fatalf("QueryAddr() error: %v", err)
	}
	return id, info
}

// resolve resolves the address and the route of id to dst from the
// device's address.
func resolve(t *testing.T, c *Channel, dev rdmatest.Dev, id uint32, uid uint64, dst netip.AddrPort) {
	t.Helper()
	if err := c.ResolveAddr(id, netip.AddrPortFrom(dev.IP, 0), dst, 2*time.Second); err != nil {
		t.Fatalf("ResolveAddr() error: %v", err)
	}
	if ev := nextEvent(t, c); ev.Type != EventAddrResolved || ev.Status != 0 || ev.UID != uid || ev.ID != id {
		t.Fatalf("after ResolveAddr: %v status %d uid %#x id %d; want ADDR_RESOLVED, status 0, uid %#x, id %d", ev.Type, ev.Status, ev.UID, ev.ID, uid, id)
	}
	if err := c.ResolveRoute(id, 2*time.Second); err != nil {
		t.Fatalf("ResolveRoute() error: %v", err)
	}
	if ev := nextEvent(t, c); ev.Type != EventRouteResolved || ev.Status != 0 || ev.UID != uid || ev.ID != id {
		t.Fatalf("after ResolveRoute: %v status %d uid %#x id %d; want ROUTE_RESOLVED, status 0, uid %#x, id %d", ev.Type, ev.Status, ev.UID, ev.ID, uid, id)
	}
}

// nodeGUID reads the node GUID of an RDMA device from sysfs, where it reads
// like "a2be:85ff:fe93:3e6a".
func nodeGUID(t *testing.T, dev string) uint64 {
	t.Helper()
	b, err := os.ReadFile(filepath.Join("/sys/class/infiniband", dev, "node_guid"))
	if err != nil {
		t.Fatal(err)
	}
	g, err := strconv.ParseUint(strings.ReplaceAll(strings.TrimSpace(string(b)), ":", ""), 16, 64)
	if err != nil {
		t.Fatal(err)
	}
	return g
}

// deviceIndex asks the rdma tool of iproute2 for the kernel's index of an
// RDMA device; without the tool it returns -1.
func deviceIndex(t *testing.T, dev string) int {
	t.Helper()
	out, err := exec.Command("rdma", "-j", "dev", "show", dev).Output()
	if err != nil {
		t.Logf("rdma -j dev show %s: %v; the device index goes unchecked", dev, err)
		return -1
	}
	var devs []struct {
		Ifindex int    `json:"ifindex"`
		Ifname  string `json:"ifname"`
	}
	if err := json.Unmarshal(out, &devs); err != nil || len(devs) != 1 || devs[0].Ifname != dev {
		t.Fatalf("rdma -j dev show %s printed %s (%v)", dev, out, err)
	}
	return devs[0].Ifindex
}

func TestKernelABIVersion(t *testing.T) {
	rdmatest.Device(t)
	if err := CheckABIVersion(); err != nil {
		t.Fatalf("CheckABIVersion() error: %v", err)
	}
}

// A connection attempt between identifiers on separate channels, from the
// bind of the listener to the reject of the request.
func TestRejectedConnectionOnTheDevice(t *testing.T) {
	dev := rdmatest.Device(t)
	const listenerUID, clientUID = 0x1001, 0x2002

	lch := openChannel(t)
	lid, bound := bindListener(t, lch, dev, listenerUID)
	if bound.Src.Addr() != dev.IP || bound.Src.Port() == 0 {
		t.Errorf("bound to %v, want %v with the port the kernel picked", bound.Src, dev.IP)
	}
	if want := nodeGUID(t, dev.Name); bound.NodeGUID != want {
		t.Errorf("node GUID %#x, want %s's %#x", bound.NodeGUID, dev.Name, want)
	}
	if bound.PortNum != uint8(dev.Port) {
		t.Errorf("port %d, want %d", bound.PortNum, dev.Port)
	}
	if want := deviceIndex(t, dev.Name); bound.DevIndex < 0 || want >= 0 && bound.DevIndex != want {
		t.Errorf("device index %d, want %d", bound.DevIndex, want)
	}
	t.Logf("bound to %v: device %s, index %d, GUID %#x, port %d, pkey %#x", bound.Src, dev.Name, bound.DevIndex, bound.NodeGUID, bound.PortNum, bound.PKey)
	if err := lch.Listen(lid, 8); err != nil {
		t.Fatalf("Listen() error: %v", err)
	}

	cch := openChannel(t)
	cid := createID(t, cch, clientUID)
	resolve(t, cch, dev, cid, clientUID, bound.Src)
	// No queue pair exists: the number is made up.
	param := ConnParam{PrivateData: []byte("ucma hello"), QPNum: 0xabcde, ResponderResources: 1, InitiatorDepth: 1, RetryCount: 7, RNRRetryCount: 7}
	if err := cch.Connect(cid, param, ECE{}); err != nil {
		t.Fatalf("Connect() error: %v", err)
	}

	req := nextEvent(t, lch)
	if req.Type != EventConnectRequest || req.Status != 0 || req.UID != listenerUID || req.ID == lid {
		t.Fatalf("listener got %v status %d uid %#x id %d; want CONNECT_REQUEST, status 0, the listener's uid %#x and a new identifier", req.Type, req.Status, req.UID, req.ID, uint64(listenerUID))
	}
	if conn := req.Conn(); !bytes.HasPrefix(conn.PrivateData, param.PrivateData) || conn.QPNum != param.QPNum {
		t.Errorf("request parameters %+v, want the private data %q and QP number %#x", conn, param.PrivateData, param.QPNum)
	}

	rch := openChannel(t)
	if _, err := rch.MigrateID(lch, req.ID); err != nil {
		t.Fatalf("MigrateID() error: %v", err)
	}
	if _, err := lch.QueryAddr(req.ID); err != syscall.EINVAL {
		t.Errorf("QueryAddr of the migrated identifier on the old channel: error %v, want EINVAL", err)
	}
	client, err := cch.QueryAddr(cid)
	if err != nil {
		t.Fatalf("QueryAddr(client) error: %v", err)
	}
	incoming, err := rch.QueryAddr(req.ID)
	if err != nil {
		t.Fatalf("QueryAddr(request) error: %v", err)
	}
	if incoming.Src != bound.Src || incoming.Dst != client.Src || client.Dst != bound.Src {
		t.Errorf("request %v <- %v, client %v -> %v; want %v <- %v and back", incoming.Src, incoming.Dst, client.Src, client.Dst, bound.Src, client.Src)
	}

	refusal := []byte("ucma no thanks")
	if err := rch.Reject(req.ID, 0, refusal); err != nil {
		t.Fatalf("Reject() error: %v", err)
	}
	rej := nextEvent(t, cch)
	if rej.Type != EventRejected || rej.Status != 28 || rej.UID != clientUID || rej.ID != cid {
		t.Errorf("client got %v status %d uid %#x id %d; want REJECTED, status 28 (consumer defined), uid %#x, id %d", rej.Type, rej.Status, rej.UID, rej.ID, uint64(clientUID), cid)
	}
	if data := rej.Conn().PrivateData; !bytes.HasPrefix(data, refusal) {
		t.Errorf("reject private data %q, want %q first", data, refusal)
	}

	for _, d := range []struct {
		name string
		c    *Channel
		id   uint32
	}{{"request", rch, req.ID}, {"client", cch, cid}, {"listener", lch, lid}} {
		n, err := d.c.DestroyID(d.id)
		if err != nil {
			t.Errorf("DestroyID(%s) error: %v", d.name, err)
		}
		t.Logf("DestroyID(%s): %d events reported", d.name, n)
	}
}

// A connection request to a port nobody listens on ends in an event, which
// the test records.
func TestConnectToAPortNobodyListensOn(t *testing.T) {
	dev := rdmatest.Device(t)
	lch := openChannel(t)
	lid, bound := bindListener(t, lch, dev, 1)
	if _, err := lch.DestroyID(lid); err != nil {
		t.Fatalf("DestroyID() error: %v", err)
	}
	cch := openChannel(t)
	cid := createID(t, cch, 2)
	resolve(t, cch, dev, cid, 2, bound.Src)
	if err := cch.Connect(cid, ConnParam{QPNum: 0xabcde, ResponderResources: 1, InitiatorDepth: 1, RetryCount: 7, RNRRetryCount: 7}, ECE{}); err != nil {
		t.Fatalf("Connect() error: %v", err)
	}
	ev := nextEvent(t, cch)
	t.Logf("a connection request to %v, where nobody listens, ends in %v with status %d", bound.Src, ev.Type, ev.Status)
	switch ev.Type {
	case EventRejected, EventUnreachable, EventConnectError:
	default:
		t.Errorf("event %v, want REJECTED, UNREACHABLE or CONNECT_ERROR", ev.Type)
	}
	if _, err := cch.DestroyID(cid); err != nil {
		t.Errorf("DestroyID() error: %v", err)
	}
}

// A hundred waiters on idle channels of the device, each with a listening
// identifier, hold no OS thread.
func TestWaitersOnTheDeviceHoldNoThreads(t *testing.T) {
	dev := rdmatest.Device(t)
	const waiters = 100
	chans := make([]*Channel, waiters)
	for i := range chans {
		chans[i] = openChannel(t)
		id, _ := bindListener(t, chans[i], dev, uint64(i+1))
		if err := chans[i].Listen(id, 1); err != nil {
			t.Fatalf("Listen() #%d error: %v", i, err)
		}
	}
	before := threadCount(t)
	ctx, cancel := context.WithCancel(context.Background())
	var wg sync.WaitGroup
	errs := make(chan error, waiters)
	for _, c := range chans {
		wg.Go(func() {
			_, err := c.GetEvent(ctx)
			errs <- err
		})
	}
	time.Sleep(500 * time.Millisecond)
	after := threadCount(t)
	cancel()
	wg.Wait()
	close(errs)
	for err := range errs {
		if err != context.Canceled {
			t.Errorf("GetEvent() error = %v, want context.Canceled", err)
		}
	}
	if after-before >= 10 {
		t.Errorf("threads grew from %d to %d with %d idle waiters, want growth < 10", before, after, waiters)
	}
}
