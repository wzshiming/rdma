//go:build linux && !android && (amd64 || arm64 || loong64 || ppc64le || riscv64 || s390x)

package ucma

import (
	"bytes"
	"testing"

	"github.com/wzshiming/rdma/internal/rdmaabi"
)

// NOTIFY carries the identifier and the event, without a response.
func TestNotifyEncoding(t *testing.T) {
	d := newFakeDevice(t)
	if err := d.ch.Notify(7, 4); err != nil {
		t.Fatalf("Notify() error: %v", err)
	}
	want := bytesOf(&rdmaabi.UcmaAbiNotify{Cmd: 15, In: 8, Id: 7, Event: 4})
	if cmds := d.commands(); len(cmds) != 1 || !bytes.Equal(cmds[0], want) {
		t.Errorf("wrote %d requests\n% x\nwant one\n% x", len(cmds), cmds, want)
	}
}
