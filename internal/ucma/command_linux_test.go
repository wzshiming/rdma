//go:build linux && !android && (amd64 || arm64 || loong64 || ppc64le || riscv64 || s390x)

package ucma

import (
	"bytes"
	"encoding/binary"
	"errors"
	"net/netip"
	"os"
	"runtime"
	"syscall"
	"testing"
	"time"

	"github.com/wzshiming/rdma/internal/rdmaabi"
)

// Arguments of the command tests.
var (
	testConn = ConnParam{PrivateData: []byte("hello"), QPNum: 0x123456, SRQ: true, ResponderResources: 2, InitiatorDepth: 3, FlowControl: 1, RetryCount: 6, RNRRetryCount: 5}
	testECE  = ECE{VendorID: 0x02c9, AttrMod: 0x1234}
)

// The request testConn and testECE make.
var (
	wantConn = rdmaabi.UcmaAbiConnParam{Qp_num: 0x123456, Private_data: privateData("hello"), Private_data_len: 5, Srq: 1, Responder_resources: 2, Initiator_depth: 3, Flow_control: 1, Retry_count: 6, Rnr_retry_count: 5, Valid: 1}
	wantECE  = rdmaabi.UcmaAbiEce{Vendor_id: 0x02c9, Attr_mod: 0x1234}
)

func privateData(s string) (d [256]byte) {
	copy(d[:], s)
	return d
}

// sockaddrIn and sockaddrIn6 build the struct sockaddr_storage of an
// address the way the kernel reads it: the family in the host's byte order,
// the port in network order, the address; flow information and scope 0.
func sockaddrIn(addr string, port uint16) (ss [128]byte) {
	ip := netip.MustParseAddr(addr).As4()
	binary.NativeEndian.PutUint16(ss[0:], syscall.AF_INET)
	binary.BigEndian.PutUint16(ss[2:], port)
	copy(ss[4:], ip[:])
	return ss
}

func sockaddrIn6(addr string, port uint16) (ss [128]byte) {
	ip := netip.MustParseAddr(addr).As16()
	binary.NativeEndian.PutUint16(ss[0:], syscall.AF_INET6)
	binary.BigEndian.PutUint16(ss[2:], port)
	copy(ss[8:], ip[:])
	return ss
}

// commandCall is a call of a command method with fixed arguments.
type commandCall struct {
	name string
	call func(c *Channel) error
	want []byte // the request the call writes; a response or value address in it is 0
	min  int    // the kernel's least response size, 0 for no response
}

// commandCalls covers every command but GET_EVENT; MigrateID moves from.
func commandCalls(t *testing.T, from *Channel) []commandCall {
	fromFD := uint32(fdOf(t, from))
	return []commandCall{
		{"CreateID", func(c *Channel) error { _, err := c.CreateID(0x1122334455667788, PortSpaceTCP, QPTypeRC); return err },
			bytesOf(&rdmaabi.UcmaAbiCreateId{Cmd: 0, In: 24, Out: 4, Uid: 0x1122334455667788, Ps: 0x0106, Qp_type: 2}), 4},
		{"CreateID UD", func(c *Channel) error { _, err := c.CreateID(1, PortSpaceUDP, QPTypeUD); return err },
			bytesOf(&rdmaabi.UcmaAbiCreateId{Cmd: 0, In: 24, Out: 4, Uid: 1, Ps: 0x0111, Qp_type: 4}), 4},
		{"DestroyID", func(c *Channel) error { _, err := c.DestroyID(7); return err },
			bytesOf(&rdmaabi.UcmaAbiDestroyId{Cmd: 1, In: 16, Out: 4, Id: 7}), 4},
		{"Bind IPv4", func(c *Channel) error { return c.Bind(7, netip.MustParseAddrPort("10.200.0.1:4791")) },
			bytesOf(&rdmaabi.UcmaAbiBind{Cmd: 20, In: 136, Id: 7, Addr_size: 16, Addr: sockaddrIn("10.200.0.1", 4791)}), 0},
		{"Bind IPv6", func(c *Channel) error { return c.Bind(7, netip.MustParseAddrPort("[2001:db8::1]:7471")) },
			bytesOf(&rdmaabi.UcmaAbiBind{Cmd: 20, In: 136, Id: 7, Addr_size: 28, Addr: sockaddrIn6("2001:db8::1", 7471)}), 0},
		{"BindIB", func(c *Channel) error { return c.BindIB(7, testIBAddr) },
			bytesOf(&rdmaabi.UcmaAbiBind{Cmd: 20, In: 136, Id: 7, Addr_size: 48, Addr: sockaddrIb("fe80::248a:703:49:d4f0", 0x8001, 0x12345, 0x0000000001061234, 0xffffffffffffffff, 3)}), 0},
		{"ResolveAddr", func(c *Channel) error {
			return c.ResolveAddr(7, netip.MustParseAddrPort("10.200.0.1:0"), netip.MustParseAddrPort("10.200.0.2:7471"), 2*time.Second)
		}, bytesOf(&rdmaabi.UcmaAbiResolveAddr{Cmd: 21, In: 272, Id: 7, Timeout_ms: 2000, Src_size: 16, Dst_size: 16, Src_addr: sockaddrIn("10.200.0.1", 0), Dst_addr: sockaddrIn("10.200.0.2", 7471)}), 0},
		{"ResolveAddr without a source", func(c *Channel) error {
			return c.ResolveAddr(7, netip.AddrPort{}, netip.MustParseAddrPort("[2001:db8::2]:7471"), 1500*time.Millisecond)
		}, bytesOf(&rdmaabi.UcmaAbiResolveAddr{Cmd: 21, In: 272, Id: 7, Timeout_ms: 1500, Dst_size: 28, Dst_addr: sockaddrIn6("2001:db8::2", 7471)}), 0},
		{"ResolveIBAddr", func(c *Channel) error {
			return c.ResolveIBAddr(7, IBAddr{GID: netip.MustParseAddr("fe80::1"), PKey: 0xffff}, testIBAddr, 2*time.Second)
		}, bytesOf(&rdmaabi.UcmaAbiResolveAddr{Cmd: 21, In: 272, Id: 7, Timeout_ms: 2000, Src_size: 48, Dst_size: 48,
			Src_addr: sockaddrIb("fe80::1", 0xffff, 0, 0, 0, 0), Dst_addr: sockaddrIb("fe80::248a:703:49:d4f0", 0x8001, 0x12345, 0x0000000001061234, 0xffffffffffffffff, 3)}), 0},
		{"ResolveIBAddr without a source", func(c *Channel) error {
			return c.ResolveIBAddr(7, IBAddr{}, testIBAddr, 1500*time.Millisecond)
		}, bytesOf(&rdmaabi.UcmaAbiResolveAddr{Cmd: 21, In: 272, Id: 7, Timeout_ms: 1500, Dst_size: 48,
			Dst_addr: sockaddrIb("fe80::248a:703:49:d4f0", 0x8001, 0x12345, 0x0000000001061234, 0xffffffffffffffff, 3)}), 0},
		{"ResolveRoute", func(c *Channel) error { return c.ResolveRoute(7, 2*time.Second) },
			bytesOf(&rdmaabi.UcmaAbiResolveRoute{Cmd: 4, In: 8, Id: 7, Timeout_ms: 2000}), 0},
		{"ResolveRoute rounds up to a millisecond", func(c *Channel) error { return c.ResolveRoute(7, 1500*time.Microsecond) },
			bytesOf(&rdmaabi.UcmaAbiResolveRoute{Cmd: 4, In: 8, Id: 7, Timeout_ms: 2}), 0},
		{"ResolveRoute stops at the kernel's int", func(c *Channel) error { return c.ResolveRoute(7, 1000*time.Hour) },
			bytesOf(&rdmaabi.UcmaAbiResolveRoute{Cmd: 4, In: 8, Id: 7, Timeout_ms: 1<<31 - 1}), 0},
		{"ResolveRoute without time", func(c *Channel) error { return c.ResolveRoute(7, -time.Second) },
			bytesOf(&rdmaabi.UcmaAbiResolveRoute{Cmd: 4, In: 8, Id: 7}), 0},
		{"QueryAddr", func(c *Channel) error { _, err := c.QueryAddr(7); return err },
			bytesOf(&rdmaabi.UcmaAbiQuery{Cmd: 19, In: 16, Out: 280, Id: 7, Option: 0}), 272},
		{"Listen", func(c *Channel) error { return c.Listen(7, 8) },
			bytesOf(&rdmaabi.UcmaAbiListen{Cmd: 7, In: 8, Id: 7, Backlog: 8}), 0},
		{"Connect", func(c *Channel) error { return c.Connect(7, testConn, testECE) },
			bytesOf(&rdmaabi.UcmaAbiConnect{Cmd: 6, In: 288, Conn_param: wantConn, Id: 7, Ece: wantECE}), 0},
		{"Accept", func(c *Channel) error { return c.Accept(7, 0x99, &testConn, testECE) },
			bytesOf(&rdmaabi.UcmaAbiAccept{Cmd: 8, In: 296, Uid: 0x99, Conn_param: wantConn, Id: 7, Ece: wantECE}), 0},
		{"Accept without parameters", func(c *Channel) error { return c.Accept(7, 0, nil, ECE{}) },
			bytesOf(&rdmaabi.UcmaAbiAccept{Cmd: 8, In: 296, Id: 7}), 0},
		{"Reject", func(c *Channel) error { return c.Reject(7, 0, []byte("no")) },
			bytesOf(&rdmaabi.UcmaAbiReject{Cmd: 9, In: 264, Id: 7, Private_data_len: 2, Private_data: privateData("no")}), 0},
		{"Reject with a reason", func(c *Channel) error { return c.Reject(7, 35, nil) },
			bytesOf(&rdmaabi.UcmaAbiReject{Cmd: 9, In: 264, Id: 7, Reason: 35}), 0},
		{"Disconnect", func(c *Channel) error { return c.Disconnect(7) },
			bytesOf(&rdmaabi.UcmaAbiDisconnect{Cmd: 10, In: 4, Id: 7}), 0},
		{"InitQPAttr", func(c *Channel) error { _, err := c.InitQPAttr(7, 3); return err },
			bytesOf(&rdmaabi.UcmaAbiInitQpAttr{Cmd: 11, In: 16, Out: 144, Id: 7, Qp_state: 3}), 144},
		{"MigrateID", func(c *Channel) error { _, err := c.MigrateID(from, 7); return err },
			bytesOf(&rdmaabi.UcmaAbiMigrateId{Cmd: 18, In: 16, Out: 4, Id: 7, Fd: fromFD}), 4},
		{"SetOption", func(c *Channel) error { return c.SetOption(7, OptionLevelID, OptionIDTOS, []byte{1}) },
			bytesOf(&rdmaabi.UcmaAbiSetOption{Cmd: 14, In: 24, Id: 7, Level: 0, Optname: 0, Optlen: 1}), 0},
		{"SetTOS", func(c *Channel) error { return c.SetTOS(7, 1) },
			bytesOf(&rdmaabi.UcmaAbiSetOption{Cmd: 14, In: 24, Id: 7, Level: 0, Optname: 0, Optlen: 1}), 0},
		{"SetReuseAddr", func(c *Channel) error { return c.SetReuseAddr(7, true) },
			bytesOf(&rdmaabi.UcmaAbiSetOption{Cmd: 14, In: 24, Id: 7, Level: 0, Optname: 1, Optlen: 4}), 0},
		{"SetAFOnly", func(c *Channel) error { return c.SetAFOnly(7, true) },
			bytesOf(&rdmaabi.UcmaAbiSetOption{Cmd: 14, In: 24, Id: 7, Level: 0, Optname: 2, Optlen: 4}), 0},
		{"SetAckTimeout", func(c *Channel) error { return c.SetAckTimeout(7, 14) },
			bytesOf(&rdmaabi.UcmaAbiSetOption{Cmd: 14, In: 24, Id: 7, Level: 0, Optname: 3, Optlen: 1}), 0},
		{"Notify", func(c *Channel) error { return c.Notify(7, 4) },
			bytesOf(&rdmaabi.UcmaAbiNotify{Cmd: 15, In: 8, Id: 7, Event: 4}), 0},
		{"JoinMulticast", func(c *Channel) error { _, err := c.JoinMulticast(7, 1, testGroup, JoinFullMember); return err },
			bytesOf(&rdmaabi.UcmaAbiJoinMcast{Cmd: 22, In: 152, Out: 4, Uid: 1, Id: 7, Addr_size: 16, Addr: sockaddrIn("239.1.1.1", 0)}), 4},
		{"LeaveMulticast", func(c *Channel) error { _, err := c.LeaveMulticast(9); return err },
			bytesOf(&rdmaabi.UcmaAbiDestroyId{Cmd: 17, In: 16, Out: 4, Id: 9}), 4},
	}
}

// Every command is one write of the whole request: the header names the
// command, in is the size of the request after the header and out the size
// of the response, which the kernel requires to be at least its minimum.
func TestCommandEncoding(t *testing.T) {
	from := newFakeDevice(t)
	for _, cc := range commandCalls(t, from.ch) {
		t.Run(cc.name, func(t *testing.T) {
			d := newFakeDevice(t)
			if err := cc.call(d.ch); err != nil {
				t.Fatalf("error: %v", err)
			}
			cmds := d.commands()
			if len(cmds) != 1 {
				t.Fatalf("%d writes, want 1", len(cmds))
			}
			got := cmds[0]
			if out := outOf(got); out != 0 {
				if out < cc.min {
					t.Errorf("response size %d, the kernel wants %d at least", out, cc.min)
				}
				off := responseOffset(opOf(got))
				if binary.NativeEndian.Uint64(got[off:]) == 0 {
					t.Error("response address 0")
				}
				clear(got[off : off+8])
			}
			if opOf(got) == cmdSetOption {
				clear(got[8:16]) // the value's address, which TestSetOptionEncoding checks
			}
			if !bytes.Equal(got, cc.want) {
				t.Errorf("wrote %d bytes\n% x\nwant %d bytes\n% x", len(got), got, len(cc.want), cc.want)
			}
		})
	}
}

// A write that takes less than the whole request fails with ENODATA, as in
// librdmacm.
func TestShortWriteIsENODATA(t *testing.T) {
	from := newFakeDevice(t)
	for _, cc := range commandCalls(t, from.ch) {
		d := newFakeDevice(t)
		d.setReply(func(fd uintptr, cmd []byte) (int, syscall.Errno) { return len(cmd) - 1, 0 })
		if err := cc.call(d.ch); err != syscall.ENODATA {
			t.Errorf("%s: error %#v, want syscall.ENODATA", cc.name, err)
		}
	}
}

// The errno of a failing write is the error.
func TestWriteErrnoIsTheError(t *testing.T) {
	from := newFakeDevice(t)
	for _, cc := range commandCalls(t, from.ch) {
		d := newFakeDevice(t)
		d.setReply(func(fd uintptr, cmd []byte) (int, syscall.Errno) { return -1, syscall.EADDRINUSE })
		if err := cc.call(d.ch); err != syscall.EADDRINUSE {
			t.Errorf("%s: error %#v, want syscall.EADDRINUSE", cc.name, err)
		}
	}
}

// Arguments no request can carry fail with EINVAL before anything is
// written.
func TestArgumentsTheRequestCannotCarry(t *testing.T) {
	long := make([]byte, 256)
	for _, tt := range []struct {
		name string
		call func(c *Channel) error
	}{
		{"Bind without an address", func(c *Channel) error { return c.Bind(7, netip.AddrPort{}) }},
		{"ResolveAddr without a destination", func(c *Channel) error { return c.ResolveAddr(7, netip.AddrPort{}, netip.AddrPort{}, time.Second) }},
		{"BindIB without a GID", func(c *Channel) error { return c.BindIB(7, IBAddr{PKey: 0xffff}) }},
		{"ResolveIBAddr without a destination", func(c *Channel) error { return c.ResolveIBAddr(7, IBAddr{}, IBAddr{}, time.Second) }},
		{"Connect with 256 bytes of private data", func(c *Channel) error { return c.Connect(7, ConnParam{PrivateData: long}, ECE{}) }},
		{"Accept with 256 bytes of private data", func(c *Channel) error { return c.Accept(7, 1, &ConnParam{PrivateData: long}, ECE{}) }},
		{"Reject with 256 bytes of private data", func(c *Channel) error { return c.Reject(7, 0, long) }},
	} {
		d := newFakeDevice(t)
		if err := tt.call(d.ch); !errors.Is(err, syscall.EINVAL) {
			t.Errorf("%s: error %v, want EINVAL", tt.name, err)
		}
		if n := len(d.commands()); n != 0 {
			t.Errorf("%s: %d writes, want none", tt.name, n)
		}
	}
	d := newFakeDevice(t)
	if err := d.ch.Reject(7, 0, long[:255]); err != nil {
		t.Errorf("Reject with 255 bytes of private data: %v", err)
	}
}

func TestCreateIDReturnsTheKernelsID(t *testing.T) {
	d := newFakeDevice(t)
	d.answer(bytesOf(&rdmaabi.UcmaAbiCreateIdResp{Id: 42}))
	if id, err := d.ch.CreateID(1, PortSpaceTCP, QPTypeRC); id != 42 || err != nil {
		t.Errorf("CreateID() = %d, %v; want 42", id, err)
	}
}

func TestDestroyIDReturnsTheEventsReported(t *testing.T) {
	d := newFakeDevice(t)
	d.answer(bytesOf(&rdmaabi.UcmaAbiDestroyIdResp{Events_reported: 3}))
	if n, err := d.ch.DestroyID(7); n != 3 || err != nil {
		t.Errorf("DestroyID() = %d, %v; want 3", n, err)
	}
}

func TestMigrateIDReturnsTheEventsReported(t *testing.T) {
	from, d := newFakeDevice(t), newFakeDevice(t)
	d.answer(bytesOf(&rdmaabi.UcmaAbiMigrateResp{Events_reported: 5}))
	if n, err := d.ch.MigrateID(from.ch, 7); n != 5 || err != nil {
		t.Errorf("MigrateID() = %d, %v; want 5", n, err)
	}
	if n := len(from.commands()); n != 0 {
		t.Errorf("%d writes to the channel migrated from, want none: the command goes to the new one", n)
	}
}

func testAddrResp() rdmaabi.UcmaAbiQueryAddrResp {
	return rdmaabi.UcmaAbiQueryAddrResp{
		Node_guid:   [8]byte{0x02, 0x00, 0x00, 0xff, 0xfe, 0x00, 0x00, 0x01},
		Port_num:    1,
		Pkey:        [2]byte{0xff, 0xff},
		Src_size:    16,
		Dst_size:    28,
		Src_addr:    sockaddrIn("10.200.0.1", 4791),
		Dst_addr:    sockaddrIn6("2001:db8::2", 7471),
		Ibdev_index: 3,
	}
}

func TestQueryAddrDecodesTheResponse(t *testing.T) {
	d := newFakeDevice(t)
	resp := testAddrResp()
	d.answer(bytesOf(&resp))
	got, err := d.ch.QueryAddr(7)
	want := AddrInfo{
		Src:      netip.MustParseAddrPort("10.200.0.1:4791"),
		Dst:      netip.MustParseAddrPort("[2001:db8::2]:7471"),
		NodeGUID: 0x020000fffe000001,
		PortNum:  1,
		PKey:     0xffff,
		DevIndex: 3,
	}
	if got != want || err != nil {
		t.Errorf("QueryAddr() = %+v, %v; want %+v", got, err, want)
	}
}

// The addresses of an identifier bound or resolved to InfiniBand addresses
// come as struct sockaddr_ib, which QueryAddr reads into SrcIB and DstIB.
func TestQueryAddrDecodesInfiniBandAddresses(t *testing.T) {
	d := newFakeDevice(t)
	resp := testAddrResp()
	resp.Src_size, resp.Dst_size = 48, 48
	resp.Src_addr = sockaddrIb("fe80::248a:703:49:d4f0", 0x8001, 0x12345, 0x0000000001061234, 0xffffffffffffffff, 3)
	resp.Dst_addr = sockaddrIb("fe80::248a:703:49:d4f1", 0x8001, 0, 0x0000000001061235, 0xffffffffffffffff, 0)
	d.answer(bytesOf(&resp))
	got, err := d.ch.QueryAddr(7)
	want := AddrInfo{
		SrcIB:    testIBAddr,
		DstIB:    IBAddr{GID: netip.MustParseAddr("fe80::248a:703:49:d4f1"), PKey: 0x8001, ServiceID: 0x0000000001061235, ServiceIDMask: 0xffffffffffffffff},
		NodeGUID: 0x020000fffe000001,
		PortNum:  1,
		PKey:     0xffff,
		DevIndex: 3,
	}
	if got != want || err != nil {
		t.Errorf("QueryAddr() = %+v, %v; want %+v", got, err, want)
	}
}

// A kernel without the device index writes the response up to ibdev_index
// only, which leaves the index unknown.
func TestQueryAddrWithoutTheDeviceIndex(t *testing.T) {
	d := newFakeDevice(t)
	resp := testAddrResp()
	d.answer(bytesOf(&resp)[:272])
	got, err := d.ch.QueryAddr(7)
	if got.DevIndex != -1 || got.NodeGUID != 0x020000fffe000001 || err != nil {
		t.Errorf("QueryAddr() = %+v, %v; want DevIndex -1 and the rest of the response", got, err)
	}
}

func testQPAttr() rdmaabi.IbUverbsQpAttr {
	return rdmaabi.IbUverbsQpAttr{
		Qp_attr_mask:    0x1fe5f,
		Qp_state:        2,
		Path_mtu:        3,
		Rq_psn:          0x123,
		Dest_qp_num:     0x456,
		Qp_access_flags: 7,
		Ah_attr: rdmaabi.IbUverbsAhAttr{
			Grh:       rdmaabi.IbUverbsGlobalRoute{Dgid: [16]byte{0xfe, 0x80, 15: 1}, Flow_label: 0x12345, Sgid_index: 1, Hop_limit: 64, Traffic_class: 3},
			Dlid:      0x10,
			Sl:        2,
			Is_global: 1,
			Port_num:  1,
		},
		Pkey_index:         4,
		Max_dest_rd_atomic: 16,
		Min_rnr_timer:      12,
		Port_num:           1,
		Timeout:            14,
	}
}

func TestInitQPAttrReturnsTheKernelsAttributes(t *testing.T) {
	d := newFakeDevice(t)
	want := testQPAttr()
	d.answer(bytesOf(&want))
	if got, err := d.ch.InitQPAttr(7, 2); got != want || err != nil {
		t.Errorf("InitQPAttr() = %+v, %v; want %+v", got, err, want)
	}
}

// growStack grows the stack of the calling goroutine, which moves it.
//
//go:noinline
func growStack(depth int) byte {
	var frame [1024]byte
	frame[depth%len(frame)] = byte(depth)
	if depth == 0 {
		return frame[0]
	}
	return growStack(depth-1) + frame[depth%len(frame)]
}

// The kernel writes a response while the write that carries its address
// runs, and the Go runtime may move or reuse memory meanwhile: the response
// must stay where the request says. The stand-in here moves the goroutine's
// stack and collects garbage before it writes the response.
func TestResponseMemoryIsPinnedForTheWrite(t *testing.T) {
	addr := testAddrResp()
	qpAttr := testQPAttr()
	for _, tt := range []struct {
		name string
		resp []byte
		call func(c *Channel) (any, error)
		want any
	}{
		{"CreateID", bytesOf(&rdmaabi.UcmaAbiCreateIdResp{Id: 42}), func(c *Channel) (any, error) { return c.CreateID(1, PortSpaceTCP, QPTypeRC) }, uint32(42)},
		{"DestroyID", bytesOf(&rdmaabi.UcmaAbiDestroyIdResp{Events_reported: 3}), func(c *Channel) (any, error) { return c.DestroyID(7) }, uint32(3)},
		{"QueryAddr", bytesOf(&addr), func(c *Channel) (any, error) { a, err := c.QueryAddr(7); return a.NodeGUID, err }, uint64(0x020000fffe000001)},
		{"InitQPAttr", bytesOf(&qpAttr), func(c *Channel) (any, error) { return c.InitQPAttr(7, 2) }, qpAttr},
		{"MigrateID", bytesOf(&rdmaabi.UcmaAbiMigrateResp{Events_reported: 5}), func(c *Channel) (any, error) {
			from := newFakeDevice(t)
			return c.MigrateID(from.ch, 7)
		}, uint32(5)},
	} {
		d := newFakeDevice(t)
		d.setReply(func(fd uintptr, cmd []byte) (int, syscall.Errno) {
			growStack(256)
			runtime.GC()
			respond(cmd, tt.resp)
			return len(cmd), 0
		})
		if got, err := tt.call(d.ch); got != tt.want || err != nil {
			t.Errorf("%s() = %v, %v; want %v", tt.name, got, err, tt.want)
		}
	}
}

// After Close no command reaches the descriptor, whose number the process
// may have given to another file by then.
func TestCommandsAfterCloseReturnErrClosed(t *testing.T) {
	from := newFakeDevice(t)
	d := newFakeDevice(t)
	if err := d.ch.Close(); err != nil {
		t.Fatal(err)
	}
	for _, cc := range commandCalls(t, from.ch) {
		if err := cc.call(d.ch); !errors.Is(err, os.ErrClosed) {
			t.Errorf("%s after Close: error %v, want os.ErrClosed", cc.name, err)
		}
	}
	if n := len(d.commands()); n != 0 {
		t.Errorf("%d writes after Close, want none", n)
	}
	// MigrateID names the descriptor it migrates from: closed, the command
	// is not sent.
	to := newFakeDevice(t)
	if _, err := to.ch.MigrateID(d.ch, 7); !errors.Is(err, os.ErrClosed) {
		t.Errorf("MigrateID from a closed channel: error %v, want os.ErrClosed", err)
	}
	if n := len(to.commands()); n != 0 {
		t.Errorf("MigrateID from a closed channel: %d writes, want none", n)
	}
}
