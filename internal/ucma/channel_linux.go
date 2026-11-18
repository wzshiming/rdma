//go:build linux && !android && (amd64 || arm64 || loong64 || ppc64le || riscv64 || s390x)

package ucma

import (
	"os"
	"runtime"
	"syscall"
	"unsafe"

	"github.com/wzshiming/rdma/internal/rdmaabi"
)

// DevicePath is the device node of the kernel's RDMA connection manager.
const DevicePath = "/dev/infiniband/rdma_cm"

// Channel is one open descriptor of the rdma_cm device, which holds the
// identifiers created through it and queues their events. It is safe for
// concurrent use.
type Channel struct {
	file *os.File
	raw  syscall.RawConn
	// write is write(2) of a command; the tests put a stand-in for the
	// kernel here.
	write func(fd uintptr, b []byte) (int, syscall.Errno)
	turn  chan struct{} // held by the GetEvent that waits on the descriptor
}

// Open opens the device: read-write, non-blocking, closed on exec.
func Open() (*Channel, error) { return open(DevicePath) }

func open(path string) (*Channel, error) {
	f, err := os.OpenFile(path, os.O_RDWR|syscall.O_NONBLOCK, 0)
	if err != nil {
		if pe, ok := err.(*os.PathError); ok {
			err = pe.Err
		}
		return nil, err
	}
	return NewChannel(f, sysWrite), nil
}

// NewChannel returns a Channel on f whose commands go to write in place of
// write(2): a stand-in for the kernel in the tests of the packages built on
// this one.
func NewChannel(f *os.File, write func(fd uintptr, b []byte) (int, syscall.Errno)) *Channel {
	raw, _ := f.SyscallConn() // fails for a nil *os.File only
	return &Channel{file: f, raw: raw, write: write, turn: make(chan struct{}, 1)}
}

func sysWrite(fd uintptr, b []byte) (int, syscall.Errno) {
	n, err := syscall.Write(int(fd), b)
	if err != nil {
		return n, err.(syscall.Errno)
	}
	return n, 0
}

// Close closes the descriptor, and with it the identifiers the channel
// holds. A command running meanwhile completes first; later calls return
// os.ErrClosed, and a second Close an error matching it.
func (c *Channel) Close() error { return c.file.Close() }

// command writes the request *cmd whole in one write; out is the size of the
// response the request points to, or 0.
func command[T any](c *Channel, op uint32, cmd *T, out uintptr) error {
	b := request(op, cmd, out)
	var n int
	var errno syscall.Errno
	if err := c.raw.Write(func(fd uintptr) bool {
		n, errno = c.write(fd, b)
		return true
	}); err != nil {
		return os.ErrClosed
	}
	return writeResult(n, errno, len(b))
}

// request fills the header of *cmd, a mirror of rdma_cma_abi.h: the command
// op, the size of the request after the header, and out, the size of the
// response. It returns the memory of the whole request.
func request[T any](op uint32, cmd *T, out uintptr) []byte {
	h := (*rdmaabi.UcmaAbiCmdHdr)(unsafe.Pointer(cmd))
	h.Cmd = op
	h.In = uint16(unsafe.Sizeof(*cmd) - unsafe.Sizeof(*h))
	h.Out = uint16(out)
	return unsafe.Slice((*byte)(unsafe.Pointer(cmd)), unsafe.Sizeof(*cmd))
}

// writeResult is the error of a command whose write of size bytes returned
// n and errno.
func writeResult(n int, errno syscall.Errno, size int) error {
	switch {
	case errno != 0:
		return errno
	case n != size:
		return syscall.ENODATA
	}
	return nil
}

// call writes the request *cmd with *resp as its response, whose address
// it stores in *response, the response field of *cmd.
func call[T, R any](c *Channel, op uint32, cmd *T, response *uint64, resp *R) error {
	var pin runtime.Pinner
	defer pin.Unpin()
	*response = pinned(&pin, resp)
	return command(c, op, cmd, unsafe.Sizeof(*resp))
}

// pinned pins resp and returns its address for the response field of a
// request. The kernel fills the response during the write, and the Go
// runtime knows nothing of an address inside a request, so the response
// stays pinned until the write returns.
func pinned[R any](pin *runtime.Pinner, resp *R) uint64 {
	pin.Pin(resp)
	return uint64(uintptr(unsafe.Pointer(resp)))
}
