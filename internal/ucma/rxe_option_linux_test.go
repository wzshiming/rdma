//go:build linux && !android && (amd64 || arm64 || loong64 || ppc64le || riscv64 || s390x)

package ucma

import (
	"syscall"
	"testing"

	"github.com/wzshiming/rdma/internal/rdmatest"
)

// Each identifier-level option takes a value of its size on a fresh
// identifier; a value of another size, an unknown name and an unknown level
// fail with the kernel's errno.
func TestOptionsOnTheDevice(t *testing.T) {
	rdmatest.Device(t)
	c := openChannel(t)
	for _, tt := range []struct {
		name string
		set  func(id uint32) error
	}{
		{"SetTOS", func(id uint32) error { return c.SetTOS(id, 0xb8) }},
		{"SetReuseAddr", func(id uint32) error { return c.SetReuseAddr(id, true) }},
		{"SetAFOnly", func(id uint32) error { return c.SetAFOnly(id, true) }},
		{"SetAckTimeout", func(id uint32) error { return c.SetAckTimeout(id, 14) }},
	} {
		if err := tt.set(createID(t, c, 0x5005)); err != nil {
			t.Errorf("%s on a fresh identifier: %v", tt.name, err)
		}
	}
	for _, tt := range []struct {
		name   string
		level  OptionLevel
		option OptionName
		value  []byte
		want   error
	}{
		{"TOS of 2 bytes", OptionLevelID, OptionIDTOS, []byte{0xb8, 0}, syscall.EINVAL},
		{"TOS without a value", OptionLevelID, OptionIDTOS, nil, syscall.EINVAL},
		{"REUSEADDR of 1 byte", OptionLevelID, OptionIDReuseAddr, []byte{1}, syscall.EINVAL},
		{"AFONLY of 8 bytes", OptionLevelID, OptionIDAFOnly, make([]byte, 8), syscall.EINVAL},
		{"ACK_TIMEOUT of 4 bytes", OptionLevelID, OptionIDAckTimeout, intValue(14), syscall.EINVAL},
		{"unknown name 99", OptionLevelID, 99, []byte{0}, syscall.ENOSYS},
		{"unknown level 7", 7, OptionIDTOS, []byte{0}, syscall.ENOSYS},
	} {
		err := c.SetOption(createID(t, c, 0x5005), tt.level, tt.option, tt.value)
		t.Logf("SetOption with %s: %v (%#v)", tt.name, err, err)
		if err != tt.want {
			t.Errorf("SetOption with %s: error %#v, want %#v", tt.name, err, tt.want)
		}
	}
}

// The kernel takes no notification for an identifier without a device.
func TestNotifyWithoutADevice(t *testing.T) {
	rdmatest.Device(t)
	c := openChannel(t)
	err := c.Notify(createID(t, c, 0x5005), 4) // IBV_EVENT_COMM_EST
	t.Logf("Notify(COMM_EST) on a fresh identifier: %v (%#v)", err, err)
	if err != syscall.EINVAL {
		t.Errorf("error %#v, want syscall.EINVAL", err)
	}
}
