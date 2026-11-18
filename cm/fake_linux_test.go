//go:build linux && !android && (amd64 || arm64 || loong64 || ppc64le || riscv64 || s390x)

package cm

import (
	"encoding/binary"
	"fmt"
	"os"
	"slices"
	"sync"
	"syscall"
	"testing"
	"unsafe"

	"github.com/wzshiming/rdma/internal/rdmaabi"
	"github.com/wzshiming/rdma/internal/ucma"
	"github.com/wzshiming/rdma/verbs"
)

// The commands a fakeKernel answers, numbered as in rdma/rdma_cma_abi.h.
const (
	opCreateID     = 0
	opDestroyID    = 1
	opResolveRoute = 4
	opListen       = 7
	opSetOption    = 14
	opMigrateID    = 18
	opQuery        = 19
	opBind         = 20
	opResolveAddr  = 21
)

var opNames = map[uint32]string{
	opCreateID: "CREATE_ID", opDestroyID: "DESTROY_ID", opResolveRoute: "RESOLVE_ROUTE", opListen: "LISTEN",
	opSetOption: "SET_OPTION", opMigrateID: "MIGRATE_ID", opQuery: "QUERY", opBind: "BIND", opResolveAddr: "RESOLVE_ADDR",
}

// fakeKernel stands in for the kernel's rdma_cm device behind the channels
// the package opens while a test runs. It records each command with the
// channel it came on, channels numbered in the order they were opened, and
// answers like the kernel: identifiers numbered from 1, the addresses of
// query for QUERY, and the errno fail returns for a command it fails.
type fakeKernel struct {
	mu       sync.Mutex
	cmds     []string
	channels int
	lastID   uint32
	query    rdmaabi.UcmaAbiQueryAddrResp
	fail     func(op uint32) syscall.Errno
}

// newFakeKernel makes the package open its channels on a new fakeKernel
// until the test ends.
func newFakeKernel(t *testing.T) *fakeKernel {
	t.Helper()
	k := new(fakeKernel)
	saved := openChannel
	openChannel = func() (*ucma.Channel, error) {
		r, w, err := os.Pipe()
		if err != nil {
			return nil, err
		}
		t.Cleanup(func() { w.Close() })
		k.mu.Lock()
		n := k.channels
		k.channels++
		k.mu.Unlock()
		return ucma.NewChannel(r, func(_ uintptr, b []byte) (int, syscall.Errno) { return k.write(n, b) }), nil
	}
	t.Cleanup(func() { openChannel = saved })
	return k
}

// failing makes the kernel fail the command op with errno.
func (k *fakeKernel) failing(op uint32, errno syscall.Errno) {
	k.mu.Lock()
	defer k.mu.Unlock()
	k.fail = func(got uint32) syscall.Errno {
		if got == op {
			return errno
		}
		return 0
	}
}

// answerQuery makes QUERY report an identifier on the device of node GUID
// guid and kernel index index, port 1.
func (k *fakeKernel) answerQuery(guid uint64, index uint32) {
	k.mu.Lock()
	defer k.mu.Unlock()
	k.query = rdmaabi.UcmaAbiQueryAddrResp{Port_num: 1, Ibdev_index: index}
	binary.BigEndian.PutUint64(k.query.Node_guid[:], guid)
}

// commands returns the commands written so far.
func (k *fakeKernel) commands() []string {
	k.mu.Lock()
	defer k.mu.Unlock()
	return slices.Clone(k.cmds)
}

func (k *fakeKernel) write(ch int, b []byte) (int, syscall.Errno) {
	op := binary.NativeEndian.Uint32(b)
	k.mu.Lock()
	defer k.mu.Unlock()
	k.cmds = append(k.cmds, fmt.Sprintf("ch%d %s", ch, render(op, b)))
	if k.fail != nil {
		if errno := k.fail(op); errno != 0 {
			return -1, errno
		}
	}
	switch op {
	case opCreateID:
		k.lastID++
		*at[rdmaabi.UcmaAbiCreateIdResp](as[rdmaabi.UcmaAbiCreateId](b).Response) = rdmaabi.UcmaAbiCreateIdResp{Id: k.lastID}
	case opQuery:
		*at[rdmaabi.UcmaAbiQueryAddrResp](as[rdmaabi.UcmaAbiQuery](b).Response) = k.query
	}
	return len(b), 0
}

// render describes a command by what the kernel reads of it; SET_OPTION's
// value as the integer it holds, read during the write as the kernel
// copies it.
func render(op uint32, b []byte) string {
	name, ok := opNames[op]
	if !ok {
		return fmt.Sprintf("command %d", op)
	}
	switch op {
	case opCreateID:
		c := as[rdmaabi.UcmaAbiCreateId](b)
		return fmt.Sprintf("%s ps=%#04x qp_type=%d", name, c.Ps, c.Qp_type)
	case opSetOption:
		c := as[rdmaabi.UcmaAbiSetOption](b)
		value := unsafe.Slice(at[byte](c.Optval), c.Optlen)
		var v any = fmt.Sprintf("% x", value)
		switch len(value) {
		case 1:
			v = value[0]
		case 4:
			v = int32(binary.NativeEndian.Uint32(value))
		}
		return fmt.Sprintf("%s id=%d level=%d name=%d len=%d value=%v", name, c.Id, c.Level, c.Optname, c.Optlen, v)
	}
	var id uint32
	switch op {
	case opDestroyID:
		id = as[rdmaabi.UcmaAbiDestroyId](b).Id
	case opResolveRoute:
		id = as[rdmaabi.UcmaAbiResolveRoute](b).Id
	case opListen:
		id = as[rdmaabi.UcmaAbiListen](b).Id
	case opMigrateID:
		id = as[rdmaabi.UcmaAbiMigrateId](b).Id
	case opQuery:
		id = as[rdmaabi.UcmaAbiQuery](b).Id
	case opBind:
		id = as[rdmaabi.UcmaAbiBind](b).Id
	case opResolveAddr:
		id = as[rdmaabi.UcmaAbiResolveAddr](b).Id
	}
	return fmt.Sprintf("%s id=%d", name, id)
}

// as reads the request b as the command structure T.
func as[T any](b []byte) *T { return (*T)(unsafe.Pointer(&b[0])) }

// at turns an address the code under test put into a request back into a
// pointer, as the kernel does with the response and value fields.
func at[T any](addr uint64) *T {
	p := uintptr(addr)
	return *(**T)(unsafe.Pointer(&p))
}

// withDevice makes the package initialised, with one device of node GUID
// guid and kernel index index, already open on a context and a protection
// domain that nothing touches, until the test ends.
func withDevice(t *testing.T, guid uint64, index int) {
	t.Helper()
	devMu.Lock()
	defer devMu.Unlock()
	savedList, savedDevices, savedReady := listDevices, devices, initialized
	d := &device{
		guid:  guid,
		index: index,
		ctx:   verbs.ContextFromPointer(unsafe.Pointer(new(rdmaabi.IbvContext))),
		pd:    verbs.PDFromPointer(unsafe.Pointer(new(uintptr))),
	}
	listDevices = func() ([]*verbs.Device, error) { return nil, nil }
	devices, initialized = []*device{d}, true
	t.Cleanup(func() {
		devMu.Lock()
		defer devMu.Unlock()
		listDevices, devices, initialized = savedList, savedDevices, savedReady
	})
}
