//go:build linux && !android && (amd64 || arm64 || loong64 || ppc64le || riscv64 || s390x)

package ucma

import (
	"bytes"
	"encoding/binary"
	"errors"
	"net/netip"
	"syscall"
	"testing"

	"github.com/wzshiming/rdma/internal/rdmaabi"
)

var testGroup = netip.MustParseAddr("239.1.1.1")

// JOIN_MCAST carries the identifier, the membership's uid, the group's
// address with its size and the join flags; LEAVE_MCAST, in the structure
// of DESTROY_ID, the membership's id. Both point to a response the kernel
// requires to be at least 4 bytes.
func TestMulticastEncoding(t *testing.T) {
	for _, cc := range []commandCall{
		{"JoinMulticast", func(c *Channel) error {
			_, err := c.JoinMulticast(7, 0x99, testGroup, JoinFullMember)
			return err
		}, bytesOf(&rdmaabi.UcmaAbiJoinMcast{Cmd: 22, In: 152, Out: 4, Uid: 0x99, Id: 7, Addr_size: 16, Addr: sockaddrIn("239.1.1.1", 0)}), 4},
		{"JoinMulticast IPv6 send-only", func(c *Channel) error {
			_, err := c.JoinMulticast(7, 0x99, netip.MustParseAddr("ff0e::1:2"), JoinSendOnlyFullMember)
			return err
		}, bytesOf(&rdmaabi.UcmaAbiJoinMcast{Cmd: 22, In: 152, Out: 4, Uid: 0x99, Id: 7, Addr_size: 28, Join_flags: 1, Addr: sockaddrIn6("ff0e::1:2", 0)}), 4},
		{"JoinMulticast v4-mapped", func(c *Channel) error {
			_, err := c.JoinMulticast(7, 0x99, netip.MustParseAddr("::ffff:239.1.1.1"), JoinFullMember)
			return err
		}, bytesOf(&rdmaabi.UcmaAbiJoinMcast{Cmd: 22, In: 152, Out: 4, Uid: 0x99, Id: 7, Addr_size: 16, Addr: sockaddrIn("239.1.1.1", 0)}), 4},
		{"LeaveMulticast", func(c *Channel) error { _, err := c.LeaveMulticast(9); return err },
			bytesOf(&rdmaabi.UcmaAbiDestroyId{Cmd: 17, In: 16, Out: 4, Id: 9}), 4},
	} {
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
			if out := outOf(got); out < cc.min {
				t.Errorf("response size %d, the kernel wants %d at least", out, cc.min)
			}
			if binary.NativeEndian.Uint64(got[8:]) == 0 {
				t.Error("response address 0")
			}
			clear(got[8:16])
			if !bytes.Equal(got, cc.want) {
				t.Errorf("wrote %d bytes\n% x\nwant %d bytes\n% x", len(got), got, len(cc.want), cc.want)
			}
		})
	}
}

func TestJoinMulticastReturnsTheMembershipID(t *testing.T) {
	d := newFakeDevice(t)
	d.answer(bytesOf(&rdmaabi.UcmaAbiCreateIdResp{Id: 42}))
	if id, err := d.ch.JoinMulticast(7, 1, testGroup, JoinFullMember); id != 42 || err != nil {
		t.Errorf("JoinMulticast() = %d, %v; want 42", id, err)
	}
}

func TestLeaveMulticastReturnsTheEventsReported(t *testing.T) {
	d := newFakeDevice(t)
	d.answer(bytesOf(&rdmaabi.UcmaAbiDestroyIdResp{Events_reported: 2}))
	if n, err := d.ch.LeaveMulticast(9); n != 2 || err != nil {
		t.Errorf("LeaveMulticast() = %d, %v; want 2", n, err)
	}
}

// A join without a group fails with EINVAL before anything is written.
func TestJoinMulticastWithoutAGroup(t *testing.T) {
	d := newFakeDevice(t)
	if _, err := d.ch.JoinMulticast(7, 1, netip.Addr{}, JoinFullMember); !errors.Is(err, syscall.EINVAL) {
		t.Errorf("error %v, want EINVAL", err)
	}
	if n := len(d.commands()); n != 0 {
		t.Errorf("%d writes, want none", n)
	}
}
