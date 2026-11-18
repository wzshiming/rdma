//go:build linux && !android && (amd64 || arm64 || loong64 || ppc64le || riscv64 || s390x)

package ucma

import (
	"bytes"
	"runtime"
	"syscall"
	"testing"

	"github.com/wzshiming/rdma/internal/rdmaabi"
)

// The kernel reads the value of an option, and writes the response of a
// join or a leave, while the write that carries its address runs. The
// stand-in moves the goroutine's stack and collects garbage first.
func TestOptionNotifyMulticastMemoryIsPinnedForTheWrite(t *testing.T) {
	for _, tt := range []struct {
		name  string
		call  func(c *Channel) error
		value []byte
	}{
		{"SetOption", func(c *Channel) error {
			return c.SetOption(7, OptionLevelIB, OptionIBPath, bytes.Repeat([]byte{0x5a}, 64))
		}, bytes.Repeat([]byte{0x5a}, 64)},
		{"SetTOS", func(c *Channel) error { return c.SetTOS(7, 0xb8) }, []byte{0xb8}},
		{"SetReuseAddr", func(c *Channel) error { return c.SetReuseAddr(7, true) }, intValue(1)},
		{"SetAFOnly", func(c *Channel) error { return c.SetAFOnly(7, true) }, intValue(1)},
		{"SetAckTimeout", func(c *Channel) error { return c.SetAckTimeout(7, 14) }, []byte{14}},
	} {
		d := newFakeDevice(t)
		var value []byte
		d.setReply(func(fd uintptr, cmd []byte) (int, syscall.Errno) {
			growStack(256)
			runtime.GC()
			value = optionValue(cmd)
			return len(cmd), 0
		})
		if err := tt.call(d.ch); err != nil || !bytes.Equal(value, tt.value) {
			t.Errorf("%s: the kernel read % x (%v), want % x", tt.name, value, err, tt.value)
		}
	}
	for _, tt := range []struct {
		name string
		resp []byte
		call func(c *Channel) (uint32, error)
		want uint32
	}{
		{"JoinMulticast", bytesOf(&rdmaabi.UcmaAbiCreateIdResp{Id: 42}), func(c *Channel) (uint32, error) {
			return c.JoinMulticast(7, 1, testGroup, JoinFullMember)
		}, 42},
		{"LeaveMulticast", bytesOf(&rdmaabi.UcmaAbiDestroyIdResp{Events_reported: 3}), func(c *Channel) (uint32, error) {
			return c.LeaveMulticast(9)
		}, 3},
	} {
		d := newFakeDevice(t)
		d.setReply(func(fd uintptr, cmd []byte) (int, syscall.Errno) {
			growStack(256)
			runtime.GC()
			respond(cmd, tt.resp)
			return len(cmd), 0
		})
		if got, err := tt.call(d.ch); got != tt.want || err != nil {
			t.Errorf("%s() = %d, %v; want %d", tt.name, got, err, tt.want)
		}
	}
}
