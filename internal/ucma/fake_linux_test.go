//go:build linux && !android && (amd64 || arm64 || loong64 || ppc64le || riscv64 || s390x)

package ucma

import (
	"bytes"
	"encoding/binary"
	"os"
	"slices"
	"sync"
	"syscall"
	"testing"
	"unsafe"

	"github.com/wzshiming/rdma/internal/rdmaabi"
)

// fakeDevice stands in for the kernel's rdma_cm device behind a Channel. The
// channel's descriptor is the read end of a pipe, readable while the fake
// has events queued, and the channel's writes of commands come to the fake,
// which records them and answers like the kernel.
type fakeDevice struct {
	ch *Channel
	r  *os.File // the channel's descriptor
	w  *os.File // a byte written here stands for a queued event

	mu     sync.Mutex
	cmds   [][]byte // copies of the commands written, oldest first
	events [][]byte // event responses for GET_EVENT, oldest first
	// reply, when set, answers instead of the default: success without a
	// response, and for GET_EVENT the oldest queued event or EAGAIN.
	reply func(fd uintptr, cmd []byte) (int, syscall.Errno)
}

func newFakeDevice(t *testing.T) *fakeDevice {
	t.Helper()
	r, w, err := os.Pipe()
	if err != nil {
		t.Fatal(err)
	}
	d := &fakeDevice{r: r, w: w}
	d.ch = NewChannel(r, d.write)
	t.Cleanup(func() {
		d.ch.Close()
		w.Close()
	})
	return d
}

func (d *fakeDevice) write(fd uintptr, cmd []byte) (int, syscall.Errno) {
	d.mu.Lock()
	d.cmds = append(d.cmds, bytes.Clone(cmd))
	reply := d.reply
	d.mu.Unlock()
	if reply != nil {
		return reply(fd, cmd)
	}
	if opOf(cmd) == cmdGetEvent {
		return d.nextEvent(fd, cmd)
	}
	return len(cmd), 0
}

// nextEvent answers GET_EVENT as the kernel does on a non-blocking
// descriptor.
func (d *fakeDevice) nextEvent(fd uintptr, cmd []byte) (int, syscall.Errno) {
	var b [1]byte
	if _, err := syscall.Read(int(fd), b[:]); err != nil {
		return -1, err.(syscall.Errno)
	}
	d.mu.Lock()
	ev := d.events[0]
	d.events = d.events[1:]
	d.mu.Unlock()
	respond(cmd, ev)
	return len(cmd), 0
}

// queue queues an event for GET_EVENT.
func (d *fakeDevice) queue(t *testing.T, ev *rdmaabi.UcmaAbiEventResp) {
	t.Helper()
	d.mu.Lock()
	d.events = append(d.events, bytes.Clone(bytesOf(ev)))
	d.mu.Unlock()
	if _, err := d.w.Write([]byte{1}); err != nil {
		t.Error(err)
	}
}

func (d *fakeDevice) setReply(reply func(fd uintptr, cmd []byte) (int, syscall.Errno)) {
	d.mu.Lock()
	defer d.mu.Unlock()
	d.reply = reply
}

// answer makes every command succeed with resp as its response.
func (d *fakeDevice) answer(resp []byte) {
	d.setReply(func(fd uintptr, cmd []byte) (int, syscall.Errno) {
		respond(cmd, resp)
		return len(cmd), 0
	})
}

// commands returns the commands written so far.
func (d *fakeDevice) commands() [][]byte {
	d.mu.Lock()
	defer d.mu.Unlock()
	return slices.Clone(d.cmds)
}

// opOf returns the command a request's header names.
func opOf(cmd []byte) uint32 { return binary.NativeEndian.Uint32(cmd) }

// outOf returns the response size a request's header gives.
func outOf(cmd []byte) int { return int(binary.NativeEndian.Uint16(cmd[6:])) }

// responseOffset is where a request holds the address of its response.
func responseOffset(op uint32) int {
	if op == cmdCreateID {
		return 16 // after the uid
	}
	return 8
}

// respond writes resp where the request's response field points, no more
// than the request's out allows, as the kernel's copy_to_user does.
func respond(cmd, resp []byte) {
	addr := binary.NativeEndian.Uint64(cmd[responseOffset(opOf(cmd)):])
	copy(unsafe.Slice(at[byte](addr), min(outOf(cmd), len(resp))), resp)
}

// at turns an address the code under test put into a request back into a
// pointer, as the kernel does with the response field.
func at[T any](addr uint64) *T {
	p := uintptr(addr)
	return *(**T)(unsafe.Pointer(&p))
}

// bytesOf returns the memory of *v.
func bytesOf[T any](v *T) []byte {
	return unsafe.Slice((*byte)(unsafe.Pointer(v)), unsafe.Sizeof(*v))
}

// fdOf returns the descriptor number of a channel.
func fdOf(t *testing.T, c *Channel) uintptr {
	t.Helper()
	var n uintptr
	if err := c.raw.Control(func(fd uintptr) { n = fd }); err != nil {
		t.Fatal(err)
	}
	return n
}
