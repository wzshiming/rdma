//go:build linux && !android && (amd64 || arm64 || loong64 || ppc64le || riscv64 || s390x)

package cm

import (
	"bytes"
	"context"
	"errors"
	"net/netip"
	"syscall"
	"testing"
	"time"

	"github.com/wzshiming/rdma/internal/rdmaabi"
	"github.com/wzshiming/rdma/internal/ucma"
	"github.com/wzshiming/rdma/verbs"
)

// asIDs adapts a resolve of datagram identifiers to resolveCommands.
func asIDs(resolve func(context.Context, netip.AddrPort, netip.AddrPort) (*DatagramID, error)) func(context.Context, netip.AddrPort, netip.AddrPort) (*ID, error) {
	return func(ctx context.Context, local, remote netip.AddrPort) (*ID, error) {
		id, err := resolve(ctx, local, remote)
		if id == nil {
			return nil, err
		}
		return id.id, err
	}
}

// asListeners adapts a listen of datagram listeners to listenCommands.
func asListeners(listen func(netip.AddrPort, int) (*DatagramListener, error)) func(netip.AddrPort, int) (*Listener, error) {
	return func(local netip.AddrPort, backlog int) (*Listener, error) {
		ln, err := listen(local, backlog)
		if ln == nil {
			return nil, err
		}
		return ln.l, err
	}
}

// checkInvalid fails t unless err is an *verbs.OpError of op matching
// syscall.EINVAL.
func checkInvalid(t *testing.T, err error, op string) {
	t.Helper()
	var opErr *verbs.OpError
	if !errors.As(err, &opErr) || opErr.Op != op || !errors.Is(err, syscall.EINVAL) {
		t.Errorf("error %v, want a *verbs.OpError of %s matching EINVAL", err, op)
	}
}

// udParam is the datagram form of an event's parameters with private data.
func udParam(data []byte) rdmaabi.UcmaAbiUdParam {
	p := rdmaabi.UcmaAbiUdParam{Private_data_len: uint8(len(data))}
	copy(p.Private_data[:], data)
	return p
}

// padded returns data at the front of n zero bytes.
func padded(data string, n int) []byte {
	b := make([]byte, n)
	copy(b, data)
	return b
}

// ResolveDatagram creates an identifier of the UDP port space, whose queue
// pairs are UD, and resolves the address as Resolve does.
func TestResolveDatagramCommands(t *testing.T) {
	checkCommands(t, resolveCommands(t, asIDs(ResolveDatagram)), []string{
		"ch0 CREATE_ID ps=0x0111 qp_type=4",
		"ch0 RESOLVE_ADDR id=1",
		"ch0 DESTROY_ID id=1",
	})
}

// ListenDatagram creates an identifier of the UDP port space and listens as
// Listen does.
func TestListenDatagramCommands(t *testing.T) {
	checkCommands(t, listenCommands(t, asListeners(ListenDatagram)), []string{
		"ch0 CREATE_ID ps=0x0111 qp_type=4",
		"ch0 BIND id=1",
		"ch0 QUERY id=1",
		"ch0 LISTEN id=1",
		"ch0 QUERY id=1",
		"ch0 DESTROY_ID id=1",
	})
}

// A Dialer and a ListenConfig send their options to a datagram identifier
// as to a connected one, right after creating it.
func TestDatagramIdentifiersTakeTheOptions(t *testing.T) {
	for _, tt := range optionCases {
		if tt.dialer.AckTimeout != nil {
			continue
		}
		t.Run(tt.name, func(t *testing.T) {
			checkCommands(t, resolveCommands(t, asIDs(tt.dialer.ResolveDatagram)), onChannel0(
				[]string{"CREATE_ID ps=0x0111 qp_type=4"},
				tt.options,
				[]string{"RESOLVE_ADDR id=1", "DESTROY_ID id=1"},
			))
		})
		t.Run(tt.name+" listening", func(t *testing.T) {
			checkCommands(t, listenCommands(t, asListeners(listenConfigOf(tt.dialer).ListenDatagram)), onChannel0(
				[]string{"CREATE_ID ps=0x0111 qp_type=4"},
				tt.options,
				[]string{"BIND id=1", "QUERY id=1", "LISTEN id=1", "QUERY id=1", "DESTROY_ID id=1"},
			))
		})
	}
}

// The kernel refuses the ACK timeout of a UD identifier, so the package
// never sends it: a Dialer or a ListenConfig with one fails before any
// command.
func TestDatagramIdentifiersRefuseTheAckTimeout(t *testing.T) {
	k := newFakeKernel(t)
	withDevice(t, fakeGUID, fakeIndex)
	d := Dialer{TOS: new(uint8(0x20)), AckTimeout: new(uint8(14))}
	_, err := d.ResolveDatagram(context.Background(), netip.AddrPort{}, fakeRemote)
	checkInvalid(t, err, "rdma_set_option")
	_, err = listenConfigOf(d).ListenDatagram(netip.MustParseAddrPort("0.0.0.0:0"), 4)
	checkInvalid(t, err, "rdma_set_option")
	checkCommands(t, k.commands(), nil)
}

// BindDatagram creates a datagram identifier and binds it to an address
// whose IP picks the device, which the QUERY that follows reports, as
// rdma_bind_addr does.
func TestBindDatagramCommands(t *testing.T) {
	k := newFakeKernel(t)
	withDevice(t, fakeGUID, fakeIndex)
	k.answerQuery(fakeGUID, fakeIndex)
	id, err := BindDatagram(netip.MustParseAddrPort("10.200.0.1:0"))
	if err != nil {
		t.Fatalf("BindDatagram() error: %v", err)
	}
	if id.Device() == nil || id.Port() != 1 {
		t.Errorf("Device() = %v, Port() = %d; want the fake device's context and port 1", id.Device(), id.Port())
	}
	if err := id.Close(); err != nil {
		t.Errorf("Close() error: %v", err)
	}
	checkCommands(t, k.commands(), []string{
		"ch0 CREATE_ID ps=0x0111 qp_type=4",
		"ch0 BIND id=1",
		"ch0 QUERY id=1",
		"ch0 DESTROY_ID id=1",
	})
}

// BindDatagram needs an IP that picks a device: an unspecified one is
// refused before any command, and a bind the kernel makes without a device
// is ENODEV.
func TestBindDatagramNeedsADevice(t *testing.T) {
	k := newFakeKernel(t)
	withDevice(t, fakeGUID, fakeIndex)
	for _, addr := range []netip.AddrPort{{}, netip.MustParseAddrPort("0.0.0.0:0"), netip.MustParseAddrPort("[::]:7174")} {
		_, err := BindDatagram(addr)
		checkInvalid(t, err, "rdma_bind_addr")
	}
	checkCommands(t, k.commands(), nil)
	_, err := BindDatagram(netip.MustParseAddrPort("10.200.0.1:0"))
	checkOpError(t, err, "rdma_bind_addr", syscall.ENODEV)
	checkCommands(t, k.commands(), []string{
		"ch0 CREATE_ID ps=0x0111 qp_type=4",
		"ch0 BIND id=1",
		"ch0 QUERY id=1",
		"ch0 DESTROY_ID id=1",
	})
}

// listenDatagramOnFake runs ListenDatagram on the wildcard address of a fake
// kernel and returns the kernel and the listener, closed when the test ends.
func listenDatagramOnFake(t *testing.T) (*fakeKernel, *DatagramListener) {
	t.Helper()
	k := newFakeKernel(t)
	withDevice(t, fakeGUID, fakeIndex)
	ln, err := ListenDatagram(netip.MustParseAddrPort("0.0.0.0:0"), 4)
	if err != nil {
		t.Fatalf("ListenDatagram() error: %v", err)
	}
	t.Cleanup(func() { ln.Close() })
	return k, ln
}

// acceptOnFake queues a request with data for identifier 9 on the listener
// of listenDatagramOnFake and accepts it.
func acceptOnFake(t *testing.T, k *fakeKernel, ln *DatagramListener, data []byte) *DatagramRequest {
	t.Helper()
	k.answerQuery(fakeGUID, fakeIndex)
	k.queue(0, udEvent(ucma.EventConnectRequest, ln.l.uid, 9, 0, udParam(data)))
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	req, err := ln.Accept(ctx)
	if err != nil {
		t.Fatalf("Accept() error: %v", err)
	}
	return req
}

// A request on a datagram listener carries the private data of the SIDR
// request, which the kernel reports in the datagram form of the event, and
// its identifier moves to a channel of its own as a connection's does.
func TestDatagramRequest(t *testing.T) {
	k, ln := listenDatagramOnFake(t)
	n := len(k.commands())
	data := padded("hello", MaxDatagramConnectPrivateData)
	req := acceptOnFake(t, k, ln, data)
	if !bytes.Equal(req.PrivateData, data) {
		t.Errorf("PrivateData = %q, want %q", req.PrivateData, data)
	}
	if req.ID.Device() == nil || req.ID.Port() != 1 {
		t.Errorf("ID.Device() = %v, ID.Port() = %d; want the fake device's context and port 1", req.ID.Device(), req.ID.Port())
	}
	if err := req.ID.Close(); err != nil {
		t.Errorf("ID.Close() error: %v", err)
	}
	checkCommands(t, k.commands()[n:], []string{
		"ch0 QUERY id=9",
		"ch1 MIGRATE_ID id=9",
		"ch1 DESTROY_ID id=9",
	})
}
