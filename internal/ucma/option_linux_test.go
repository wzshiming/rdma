//go:build linux && !android && (amd64 || arm64 || loong64 || ppc64le || riscv64 || s390x)

package ucma

import (
	"bytes"
	"encoding/binary"
	"syscall"
	"testing"
	"unsafe"

	"github.com/wzshiming/rdma/internal/rdmaabi"
)

// intValue is the memory of a C int holding v.
func intValue(v int32) []byte { return binary.NativeEndian.AppendUint32(nil, uint32(v)) }

// optionValue reads the value a SET_OPTION request points to, as the
// kernel copies it during the write.
func optionValue(cmd []byte) []byte {
	optval := binary.NativeEndian.Uint64(cmd[8:])
	optlen := binary.NativeEndian.Uint32(cmd[28:])
	if optlen == 0 {
		return nil
	}
	return bytes.Clone(unsafe.Slice(at[byte](optval), optlen))
}

// SET_OPTION carries the level, the name and the length of the value with
// its address, which is 0 for no value.
func TestSetOptionEncoding(t *testing.T) {
	for _, tt := range []struct {
		name  string
		call  func(c *Channel) error
		want  rdmaabi.UcmaAbiSetOption // optval 0
		value []byte
	}{
		{"SetOption", func(c *Channel) error { return c.SetOption(7, OptionLevelIB, OptionIBPath, []byte{1, 2, 3}) },
			rdmaabi.UcmaAbiSetOption{Cmd: 14, In: 24, Id: 7, Level: 1, Optname: 1, Optlen: 3}, []byte{1, 2, 3}},
		{"SetOption without a value", func(c *Channel) error { return c.SetOption(7, OptionLevelID, OptionIDTOS, nil) },
			rdmaabi.UcmaAbiSetOption{Cmd: 14, In: 24, Id: 7}, nil},
		{"SetTOS", func(c *Channel) error { return c.SetTOS(7, 0xb8) },
			rdmaabi.UcmaAbiSetOption{Cmd: 14, In: 24, Id: 7, Level: 0, Optname: 0, Optlen: 1}, []byte{0xb8}},
		{"SetReuseAddr", func(c *Channel) error { return c.SetReuseAddr(7, true) },
			rdmaabi.UcmaAbiSetOption{Cmd: 14, In: 24, Id: 7, Level: 0, Optname: 1, Optlen: 4}, intValue(1)},
		{"SetReuseAddr false", func(c *Channel) error { return c.SetReuseAddr(7, false) },
			rdmaabi.UcmaAbiSetOption{Cmd: 14, In: 24, Id: 7, Level: 0, Optname: 1, Optlen: 4}, intValue(0)},
		{"SetAFOnly", func(c *Channel) error { return c.SetAFOnly(7, true) },
			rdmaabi.UcmaAbiSetOption{Cmd: 14, In: 24, Id: 7, Level: 0, Optname: 2, Optlen: 4}, intValue(1)},
		{"SetAckTimeout", func(c *Channel) error { return c.SetAckTimeout(7, 14) },
			rdmaabi.UcmaAbiSetOption{Cmd: 14, In: 24, Id: 7, Level: 0, Optname: 3, Optlen: 1}, []byte{14}},
	} {
		t.Run(tt.name, func(t *testing.T) {
			d := newFakeDevice(t)
			var value []byte
			d.setReply(func(fd uintptr, cmd []byte) (int, syscall.Errno) {
				value = optionValue(cmd)
				return len(cmd), 0
			})
			if err := tt.call(d.ch); err != nil {
				t.Fatalf("error: %v", err)
			}
			cmds := d.commands()
			if len(cmds) != 1 {
				t.Fatalf("%d writes, want 1", len(cmds))
			}
			got := cmds[0]
			if optval := binary.NativeEndian.Uint64(got[8:]); (optval != 0) != (len(tt.value) != 0) {
				t.Errorf("value address %#x for %d bytes of value", optval, len(tt.value))
			}
			clear(got[8:16])
			if want := bytesOf(&tt.want); !bytes.Equal(got, want) {
				t.Errorf("wrote %d bytes\n% x\nwant %d bytes\n% x", len(got), got, len(want), want)
			}
			if !bytes.Equal(value, tt.value) {
				t.Errorf("the value read during the write is % x, want % x", value, tt.value)
			}
		})
	}
}
