//go:build linux && !android && (amd64 || arm64 || loong64 || ppc64le || riscv64 || s390x)

package ucma

import (
	"bytes"
	"encoding/binary"
	"slices"
	"strconv"
	"syscall"
	"testing"

	"github.com/wzshiming/rdma/internal/rdmaabi"
)

// commandName names a request by its command, a query by its option too.
func commandName(cmd []byte) string {
	switch opOf(cmd) {
	case cmdCreateID:
		return "CREATE_ID"
	case cmdDestroyID:
		return "DESTROY_ID"
	case cmdBind:
		return "BIND"
	case cmdQuery:
		switch binary.NativeEndian.Uint32(cmd[20:]) {
		case queryAddr:
			return "QUERY ADDR"
		case queryGID:
			return "QUERY GID"
		}
		return "QUERY"
	}
	return strconv.Itoa(int(opOf(cmd)))
}

// probeKernel makes d answer like a kernel whose commands named in errs fail
// with their errno: CREATE_ID makes identifier 42 and the rest succeed.
func probeKernel(d *fakeDevice, errs map[string]syscall.Errno) {
	d.setReply(func(fd uintptr, cmd []byte) (int, syscall.Errno) {
		if errno := errs[commandName(cmd)]; errno != 0 {
			return -1, errno
		}
		if opOf(cmd) == cmdCreateID {
			respond(cmd, bytesOf(&rdmaabi.UcmaAbiCreateIdResp{Id: 42}))
		}
		return len(cmd), 0
	})
}

// The probe is librdmacm's: an identifier of the IB port space with the RC
// type binds to the any address of AF_IB in the TCP port space, with the
// service ID RDMA_IB_IP_PS_TCP under the mask RDMA_IB_IP_PS_MASK, and its
// addresses and GIDs are queried, as rdma_bind_addr does after a bind; the
// identifier is destroyed.
func TestProbeAFIBBindsTheAnyAddressOfTheTCPPortSpace(t *testing.T) {
	d := newFakeDevice(t)
	probeKernel(d, nil)
	if !d.ch.ProbeAFIB() {
		t.Error("ProbeAFIB() = false with every command succeeding, want true")
	}
	cmds := d.commands()
	var names []string
	for _, cmd := range cmds {
		names = append(names, commandName(cmd))
	}
	if want := []string{"CREATE_ID", "BIND", "QUERY ADDR", "QUERY GID", "DESTROY_ID"}; !slices.Equal(names, want) {
		t.Fatalf("commands %q, want %q", names, want)
	}
	var create rdmaabi.UcmaAbiCreateId
	copy(bytesOf(&create), cmds[0])
	if create.Ps != uint16(PortSpaceIB) || create.Qp_type != uint8(QPTypeRC) || create.Uid == 0 {
		t.Errorf("CREATE_ID in port space %#x with QP type %d and uid %#x; want %#x, %d and a uid other than 0", create.Ps, create.Qp_type, create.Uid, uint16(PortSpaceIB), QPTypeRC)
	}
	bind := bytesOf(&rdmaabi.UcmaAbiBind{Cmd: 20, In: 136, Id: 42, Addr_size: 48, Addr: sockaddrIb("::", 0, 0, 0x0000000001060000, 0xffffffffffff0000, 0)})
	if !bytes.Equal(cmds[1], bind) {
		t.Errorf("BIND wrote\n% x\nwant\n% x", cmds[1], bind)
	}
	for _, cmd := range cmds[2:4] {
		var query rdmaabi.UcmaAbiQuery
		copy(bytesOf(&query), cmd)
		if query.Id != 42 {
			t.Errorf("%s of identifier %d, want 42", commandName(cmd), query.Id)
		}
	}
	var destroy rdmaabi.UcmaAbiDestroyId
	copy(bytesOf(&destroy), cmds[4])
	if destroy.Id != 42 {
		t.Errorf("DESTROY_ID of identifier %d, want 42", destroy.Id)
	}
}

// Any failure means no: of CREATE_ID, of the bind, or of a query after it.
// The identifier is destroyed whenever it was made.
func TestProbeAFIBFailures(t *testing.T) {
	for _, tt := range []struct {
		fail  string
		errno syscall.Errno
		want  []string
	}{
		{"CREATE_ID", syscall.ENODEV, []string{"CREATE_ID"}},
		{"BIND", syscall.EAFNOSUPPORT, []string{"CREATE_ID", "BIND", "DESTROY_ID"}},
		{"QUERY ADDR", syscall.ENOSYS, []string{"CREATE_ID", "BIND", "QUERY ADDR", "DESTROY_ID"}},
		{"QUERY GID", syscall.ENOSYS, []string{"CREATE_ID", "BIND", "QUERY ADDR", "QUERY GID", "DESTROY_ID"}},
	} {
		d := newFakeDevice(t)
		probeKernel(d, map[string]syscall.Errno{tt.fail: tt.errno})
		if d.ch.ProbeAFIB() {
			t.Errorf("ProbeAFIB() = true with %s failing, want false", tt.fail)
		}
		var names []string
		for _, cmd := range d.commands() {
			names = append(names, commandName(cmd))
		}
		if !slices.Equal(names, tt.want) {
			t.Errorf("with %s failing: commands %q, want %q", tt.fail, names, tt.want)
		}
	}
}
