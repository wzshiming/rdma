//go:build linux && !android && (amd64 || arm64 || loong64 || ppc64le || riscv64 || s390x)

package cm

import (
	"context"
	"net/netip"
	"slices"
	"strings"
	"syscall"
	"testing"

	"github.com/wzshiming/rdma/internal/ucma"
	"github.com/wzshiming/rdma/verbs"
)

// The fake device of the command tests: the node GUID and kernel index
// QUERY reports for a request.
const fakeGUID, fakeIndex = 0x0002c903000a0b0c, 3

var fakeRemote = netip.MustParseAddrPort("10.200.0.2:4791")

func checkCommands(t *testing.T, got, want []string) {
	t.Helper()
	if !slices.Equal(got, want) {
		t.Errorf("commands:\n\t%s\nwant:\n\t%s", strings.Join(got, "\n\t"), strings.Join(want, "\n\t"))
	}
}

func checkOpError(t *testing.T, err error, op string, errno syscall.Errno) {
	t.Helper()
	opErr, ok := err.(*verbs.OpError)
	if !ok || opErr.Op != op || opErr.Err != errno {
		t.Errorf("error %#v, want a *verbs.OpError of %s with %v", err, op, errno)
	}
}

// resolveCommands runs resolve on a fake kernel that refuses RESOLVE_ADDR
// and returns the commands it sent.
func resolveCommands(t *testing.T, resolve func(context.Context, netip.AddrPort, netip.AddrPort) (*ID, error)) []string {
	t.Helper()
	k := newFakeKernel(t)
	withDevice(t, fakeGUID, fakeIndex)
	k.failing(opResolveAddr, syscall.ENETUNREACH)
	_, err := resolve(context.Background(), netip.AddrPort{}, fakeRemote)
	checkOpError(t, err, "rdma_resolve_addr", syscall.ENETUNREACH)
	return k.commands()
}

// listenOnFake runs listen on the wildcard address of a fake kernel and
// returns the kernel and the Listener, closed when the test ends.
func listenOnFake(t *testing.T, listen func(netip.AddrPort, int) (*Listener, error)) (*fakeKernel, *Listener) {
	t.Helper()
	k := newFakeKernel(t)
	withDevice(t, fakeGUID, fakeIndex)
	ln, err := listen(netip.MustParseAddrPort("0.0.0.0:0"), 4)
	if err != nil {
		t.Fatalf("Listen() error: %v", err)
	}
	t.Cleanup(func() { ln.Close() })
	return k, ln
}

// listenCommands runs listen on a fake kernel, closes the Listener and
// returns the commands sent.
func listenCommands(t *testing.T, listen func(netip.AddrPort, int) (*Listener, error)) []string {
	t.Helper()
	k, ln := listenOnFake(t, listen)
	if err := ln.Close(); err != nil {
		t.Errorf("Close() error: %v", err)
	}
	return k.commands()
}

// requestEvent is the CONNECT_REQUEST event of identifier id on ln.
func requestEvent(ln *Listener, id uint32) ucma.Event {
	return ucma.Event{Type: ucma.EventConnectRequest, UID: ln.uid, ID: id}
}

// takeRequest hands a Listener of listen on a fake kernel a request for
// identifier 9 and returns the request's commands, those of closing its ID
// included.
func takeRequest(t *testing.T, listen func(netip.AddrPort, int) (*Listener, error)) []string {
	t.Helper()
	k, ln := listenOnFake(t, listen)
	n := len(k.commands())
	k.answerQuery(fakeGUID, fakeIndex)
	req, err := ln.request(requestEvent(ln, 9))
	if err != nil || req == nil {
		t.Fatalf("request() = %v, %v; want a request", req, err)
	}
	if err := req.ID.Close(); err != nil {
		t.Errorf("ID.Close() error: %v", err)
	}
	return k.commands()[n:]
}

// Resolve creates an RC identifier of the TCP port space, then resolves the
// address; a RESOLVE_ADDR the kernel refuses destroys the identifier.
func TestResolveCommands(t *testing.T) {
	checkCommands(t, resolveCommands(t, Resolve), []string{
		"ch0 CREATE_ID ps=0x0106 qp_type=2",
		"ch0 RESOLVE_ADDR id=1",
		"ch0 DESTROY_ID id=1",
	})
}

// Listen creates an RC identifier of the TCP port space, binds it, checks
// its device and listens, then reads the address it is bound to.
func TestListenCommands(t *testing.T) {
	checkCommands(t, listenCommands(t, Listen), []string{
		"ch0 CREATE_ID ps=0x0106 qp_type=2",
		"ch0 BIND id=1",
		"ch0 QUERY id=1",
		"ch0 LISTEN id=1",
		"ch0 QUERY id=1",
		"ch0 DESTROY_ID id=1",
	})
}

// A request's identifier is queried on the listener's channel and moved to
// a channel of its own.
func TestRequestCommands(t *testing.T) {
	checkCommands(t, takeRequest(t, Listen), []string{
		"ch0 QUERY id=9",
		"ch1 MIGRATE_ID id=9",
		"ch1 DESTROY_ID id=9",
	})
}
