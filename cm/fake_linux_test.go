//go:build linux && !android && (amd64 || arm64 || loong64 || ppc64le || riscv64 || s390x)

package cm

import (
	"encoding/binary"
	"fmt"
	"net/netip"
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
	opConnect      = 6
	opListen       = 7
	opAccept       = 8
	opReject       = 9
	opInitQPAttr   = 11
	opGetEvent     = 12
	opSetOption    = 14
	opLeaveMcast   = 17
	opMigrateID    = 18
	opQuery        = 19
	opBind         = 20
	opResolveAddr  = 21
	opJoinMcast    = 22
)

var opNames = map[uint32]string{
	opCreateID: "CREATE_ID", opDestroyID: "DESTROY_ID", opResolveRoute: "RESOLVE_ROUTE", opConnect: "CONNECT", opListen: "LISTEN",
	opAccept: "ACCEPT", opReject: "REJECT", opInitQPAttr: "INIT_QP_ATTR", opSetOption: "SET_OPTION", opLeaveMcast: "LEAVE_MCAST",
	opMigrateID: "MIGRATE_ID", opQuery: "QUERY", opBind: "BIND", opResolveAddr: "RESOLVE_ADDR", opJoinMcast: "JOIN_MCAST",
}

// fakeKernel stands in for the kernel's rdma_cm device behind the channels
// the package opens while a test runs. It records each command but
// GET_EVENT with the channel it came on, channels numbered in the order they
// were opened, and answers like the kernel: identifiers and memberships
// numbered from 1, the addresses of query for QUERY, qpAttr for
// INIT_QP_ATTR, the events queued on the channel for GET_EVENT, and the
// errno fail returns for a command it fails. A command with a reaction runs
// it once answered, which may queue the events the kernel would report.
type fakeKernel struct {
	mu        sync.Mutex
	cmds      []string
	channels  int
	pipes     []*os.File                        // the write end of each channel's pipe, readable while it has events
	events    [][]rdmaabi.UcmaAbiEventResp      // queued on each channel, oldest first
	uids      []uint64                          // of the last identifier created on each channel
	reactions map[uint32]func(ch int, b []byte) // by command
	lastID    uint32
	lastMC    uint32
	query     rdmaabi.UcmaAbiQueryAddrResp
	qpAttr    rdmaabi.IbUverbsQpAttr
	fail      func(op uint32) syscall.Errno
}

// newFakeKernel makes the package open its channels on a new fakeKernel
// until the test ends.
func newFakeKernel(t *testing.T) *fakeKernel {
	t.Helper()
	k := &fakeKernel{reactions: make(map[uint32]func(int, []byte))}
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
		k.pipes = append(k.pipes, w)
		k.events = append(k.events, nil)
		k.uids = append(k.uids, 0)
		k.mu.Unlock()
		return ucma.NewChannel(r, func(fd uintptr, b []byte) (int, syscall.Errno) { return k.write(n, fd, b) }), nil
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

// on makes the kernel run react once it has answered the command op.
func (k *fakeKernel) on(op uint32, react func(ch int, b []byte)) {
	k.mu.Lock()
	defer k.mu.Unlock()
	k.reactions[op] = react
}

// uidOn returns the uid of the last identifier created on channel ch.
func (k *fakeKernel) uidOn(ch int) uint64 {
	k.mu.Lock()
	defer k.mu.Unlock()
	return k.uids[ch]
}

// queue queues ev on channel ch for GET_EVENT.
func (k *fakeKernel) queue(ch int, ev rdmaabi.UcmaAbiEventResp) {
	k.mu.Lock()
	k.events[ch] = append(k.events[ch], ev)
	w := k.pipes[ch]
	k.mu.Unlock()
	w.Write([]byte{1})
}

func (k *fakeKernel) write(ch int, fd uintptr, b []byte) (int, syscall.Errno) {
	op := binary.NativeEndian.Uint32(b)
	if op == opGetEvent {
		return k.nextEvent(ch, fd, b)
	}
	k.mu.Lock()
	k.cmds = append(k.cmds, fmt.Sprintf("ch%d %s", ch, render(op, b)))
	if k.fail != nil {
		if errno := k.fail(op); errno != 0 {
			k.mu.Unlock()
			return -1, errno
		}
	}
	switch op {
	case opCreateID:
		k.lastID++
		c := as[rdmaabi.UcmaAbiCreateId](b)
		k.uids[ch] = c.Uid
		*at[rdmaabi.UcmaAbiCreateIdResp](c.Response) = rdmaabi.UcmaAbiCreateIdResp{Id: k.lastID}
	case opQuery:
		*at[rdmaabi.UcmaAbiQueryAddrResp](as[rdmaabi.UcmaAbiQuery](b).Response) = k.query
	case opInitQPAttr:
		*at[rdmaabi.IbUverbsQpAttr](as[rdmaabi.UcmaAbiInitQpAttr](b).Response) = k.qpAttr
	case opJoinMcast:
		k.lastMC++
		*at[rdmaabi.UcmaAbiCreateIdResp](as[rdmaabi.UcmaAbiJoinMcast](b).Response) = rdmaabi.UcmaAbiCreateIdResp{Id: k.lastMC}
	}
	react := k.reactions[op]
	k.mu.Unlock()
	if react != nil {
		react(ch, b)
	}
	return len(b), 0
}

// nextEvent answers GET_EVENT as the kernel does on a non-blocking
// descriptor: with the oldest event queued on the channel, or EAGAIN.
func (k *fakeKernel) nextEvent(ch int, fd uintptr, b []byte) (int, syscall.Errno) {
	var one [1]byte
	if _, err := syscall.Read(int(fd), one[:]); err != nil {
		return -1, err.(syscall.Errno)
	}
	k.mu.Lock()
	ev := k.events[ch][0]
	k.events[ch] = k.events[ch][1:]
	k.mu.Unlock()
	*at[rdmaabi.UcmaAbiEventResp](as[rdmaabi.UcmaAbiGetEvent](b).Response) = ev
	return len(b), 0
}

// udEvent is an event of type typ with status for the identifier or
// membership uid, id, with p in the datagram form of the parameters.
func udEvent(typ ucma.EventType, uid uint64, id uint32, status int32, p rdmaabi.UcmaAbiUdParam) rdmaabi.UcmaAbiEventResp {
	ev := rdmaabi.UcmaAbiEventResp{Uid: uid, Id: id, Event: uint32(typ), Status: uint32(status)}
	*(*rdmaabi.UcmaAbiUdParam)(unsafe.Pointer(&ev.Param)) = p
	return ev
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
	case opConnect:
		c := as[rdmaabi.UcmaAbiConnect](b)
		return fmt.Sprintf("%s id=%d %s ece=%#x/%#x", name, c.Id, renderConn(&c.Conn_param), c.Ece.Vendor_id, c.Ece.Attr_mod)
	case opAccept:
		c := as[rdmaabi.UcmaAbiAccept](b)
		return fmt.Sprintf("%s id=%d uid=%#x %s ece=%#x/%#x", name, c.Id, c.Uid, renderConn(&c.Conn_param), c.Ece.Vendor_id, c.Ece.Attr_mod)
	case opReject:
		c := as[rdmaabi.UcmaAbiReject](b)
		return fmt.Sprintf("%s id=%d reason=%d data=%x", name, c.Id, c.Reason, c.Private_data[:c.Private_data_len])
	case opInitQPAttr:
		c := as[rdmaabi.UcmaAbiInitQpAttr](b)
		return fmt.Sprintf("%s id=%d state=%d", name, c.Id, c.Qp_state)
	case opJoinMcast:
		c := as[rdmaabi.UcmaAbiJoinMcast](b)
		return fmt.Sprintf("%s id=%d uid=%#x addr=%v size=%d flags=%d", name, c.Id, c.Uid, sockaddrIP(c.Addr[:]), c.Addr_size, c.Join_flags)
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
	case opLeaveMcast:
		id = as[rdmaabi.UcmaAbiDestroyId](b).Id
	}
	return fmt.Sprintf("%s id=%d", name, id)
}

// renderConn describes the connection form of the parameters of CONNECT and
// ACCEPT.
func renderConn(p *rdmaabi.UcmaAbiConnParam) string {
	return fmt.Sprintf("qp=%#x srq=%d depths=%d/%d flow=%d retries=%d/%d valid=%d data=%x",
		p.Qp_num, p.Srq, p.Responder_resources, p.Initiator_depth, p.Flow_control, p.Retry_count, p.Rnr_retry_count, p.Valid,
		p.Private_data[:p.Private_data_len])
}

// sockaddrIP reads the IP address of the struct sockaddr_in or sockaddr_in6
// at the front of b.
func sockaddrIP(b []byte) netip.Addr {
	switch binary.NativeEndian.Uint16(b) {
	case syscall.AF_INET:
		return netip.AddrFrom4([4]byte(b[4:8]))
	case syscall.AF_INET6:
		return netip.AddrFrom16([16]byte(b[8:24]))
	}
	return netip.Addr{}
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
