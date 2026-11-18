//go:build linux && !android && (amd64 || arm64 || loong64 || ppc64le || riscv64 || s390x)

package cm

import (
	"context"
	"errors"
	"net/netip"
	"syscall"
	"testing"

	"github.com/wzshiming/rdma/internal/ucma"
	"github.com/wzshiming/rdma/verbs"
)

// optionCases are configurations of the options, each with the SET_OPTION
// commands it sends on identifier 1 in rdma_set_option's encoding: level
// RDMA_OPTION_ID, the name, a uint8_t for TOS and ACK_TIMEOUT, an int for
// REUSEADDR and AFONLY; none for an option left unset.
var optionCases = []struct {
	name    string
	dialer  Dialer
	options []string
}{
	{"zero", Dialer{}, nil},
	{"TOS", Dialer{TOS: new(uint8(0xb8))}, []string{"SET_OPTION id=1 level=0 name=0 len=1 value=184"}},
	{"TOS 0", Dialer{TOS: new(uint8(0))}, []string{"SET_OPTION id=1 level=0 name=0 len=1 value=0"}},
	{"ReuseAddr", Dialer{ReuseAddr: true}, []string{"SET_OPTION id=1 level=0 name=1 len=4 value=1"}},
	{"AFOnly false", Dialer{AFOnly: new(false)}, []string{"SET_OPTION id=1 level=0 name=2 len=4 value=0"}},
	{"AFOnly true", Dialer{AFOnly: new(true)}, []string{"SET_OPTION id=1 level=0 name=2 len=4 value=1"}},
	{"AckTimeout 0", Dialer{AckTimeout: new(uint8(0))}, []string{"SET_OPTION id=1 level=0 name=3 len=1 value=0"}},
	{"AckTimeout 31", Dialer{AckTimeout: new(uint8(31))}, []string{"SET_OPTION id=1 level=0 name=3 len=1 value=31"}},
	{"all", Dialer{TOS: new(uint8(0x20)), ReuseAddr: true, AFOnly: new(true), AckTimeout: new(uint8(14))}, []string{
		"SET_OPTION id=1 level=0 name=0 len=1 value=32",
		"SET_OPTION id=1 level=0 name=1 len=4 value=1",
		"SET_OPTION id=1 level=0 name=2 len=4 value=1",
		"SET_OPTION id=1 level=0 name=3 len=1 value=14",
	}},
}

// listenConfigOf is the ListenConfig with the options of d.
func listenConfigOf(d Dialer) ListenConfig {
	return ListenConfig{TOS: d.TOS, ReuseAddr: d.ReuseAddr, AFOnly: d.AFOnly, AckTimeout: d.AckTimeout}
}

// onChannel0 puts the commands on the first channel.
func onChannel0(cmds ...[]string) []string {
	var all []string
	for _, c := range cmds {
		for _, cmd := range c {
			all = append(all, "ch0 "+cmd)
		}
	}
	return all
}

// A Dialer sends its options right after creating the identifier, so
// before RESOLVE_ADDR binds it and resolves the address, which comes before
// the route and the connection.
func TestDialerSendsItsOptionsBeforeResolving(t *testing.T) {
	for _, tt := range optionCases {
		t.Run(tt.name, func(t *testing.T) {
			checkCommands(t, resolveCommands(t, tt.dialer.Resolve), onChannel0(
				[]string{"CREATE_ID ps=0x0106 qp_type=2"},
				tt.options,
				[]string{"RESOLVE_ADDR id=1", "DESTROY_ID id=1"},
			))
		})
	}
}

// A ListenConfig sends its options right after creating the identifier, so
// before BIND and LISTEN.
func TestListenConfigSendsItsOptionsBeforeBinding(t *testing.T) {
	for _, tt := range optionCases {
		t.Run(tt.name, func(t *testing.T) {
			checkCommands(t, listenCommands(t, listenConfigOf(tt.dialer).Listen), onChannel0(
				[]string{"CREATE_ID ps=0x0106 qp_type=2"},
				tt.options,
				[]string{"BIND id=1", "QUERY id=1", "LISTEN id=1", "QUERY id=1", "DESTROY_ID id=1"},
			))
		})
	}
}

// The kernel gives a request's identifier none of its listener's options.
// The Listener sets its ACK timeout on it once the identifier is on a
// channel of its own, before Accept can take the queue pair to RTS; the
// other options have no step left on a request.
func TestRequestTakesTheAckTimeoutOfItsListener(t *testing.T) {
	for _, tt := range []struct {
		name   string
		config ListenConfig
		want   []string
	}{
		{"zero", ListenConfig{}, nil},
		{"AckTimeout", ListenConfig{AckTimeout: new(uint8(16))}, []string{"ch1 SET_OPTION id=9 level=0 name=3 len=1 value=16"}},
		{"AckTimeout 0", ListenConfig{AckTimeout: new(uint8(0))}, []string{"ch1 SET_OPTION id=9 level=0 name=3 len=1 value=0"}},
		{"all but AckTimeout", ListenConfig{TOS: new(uint8(0x20)), ReuseAddr: true, AFOnly: new(true)}, nil},
	} {
		t.Run(tt.name, func(t *testing.T) {
			want := append([]string{"ch0 QUERY id=9", "ch1 MIGRATE_ID id=9"}, tt.want...)
			checkCommands(t, takeRequest(t, tt.config.Listen), append(want, "ch1 DESTROY_ID id=9"))
		})
	}
}

// Resolve sends the values it took, not those the caller's variables hold
// once it has taken them: here from the opening of its channel on.
func TestResolveSendsTheValuesItTook(t *testing.T) {
	tos, afOnly, timeout := uint8(0x20), true, uint8(14)
	resolve := func(ctx context.Context, local, remote netip.AddrPort) (*ID, error) {
		open := openChannel
		openChannel = func() (*ucma.Channel, error) {
			tos, afOnly, timeout = 0xff, false, 200
			return open()
		}
		return Dialer{TOS: &tos, AFOnly: &afOnly, AckTimeout: &timeout}.Resolve(ctx, local, remote)
	}
	checkCommands(t, resolveCommands(t, resolve), onChannel0([]string{
		"CREATE_ID ps=0x0106 qp_type=2",
		"SET_OPTION id=1 level=0 name=0 len=1 value=32",
		"SET_OPTION id=1 level=0 name=2 len=4 value=1",
		"SET_OPTION id=1 level=0 name=3 len=1 value=14",
		"RESOLVE_ADDR id=1",
		"DESTROY_ID id=1",
	}))
}

// A request takes the ACK timeout Listen took, not the one the caller's
// variable holds afterwards.
func TestRequestTakesTheAckTimeoutListenTook(t *testing.T) {
	timeout := uint8(16)
	listen := func(local netip.AddrPort, backlog int) (*Listener, error) {
		ln, err := ListenConfig{AckTimeout: &timeout}.Listen(local, backlog)
		timeout = 200
		return ln, err
	}
	checkCommands(t, takeRequest(t, listen), []string{
		"ch0 QUERY id=9",
		"ch1 MIGRATE_ID id=9",
		"ch1 SET_OPTION id=9 level=0 name=3 len=1 value=16",
		"ch1 DESTROY_ID id=9",
	})
}

// An option the kernel refuses fails the call with an *verbs.OpError of
// rdma_set_option carrying the kernel's errno; the identifier is destroyed
// and nothing else is sent.
func TestRefusedOptionIsAnOpErrorOfSetOption(t *testing.T) {
	t.Run("Dialer", func(t *testing.T) {
		k := newFakeKernel(t)
		withDevice(t, fakeGUID, fakeIndex)
		k.failing(opSetOption, syscall.ENOSYS)
		_, err := Dialer{AckTimeout: new(uint8(14))}.Resolve(context.Background(), netip.AddrPort{}, fakeRemote)
		checkOpError(t, err, "rdma_set_option", syscall.ENOSYS)
		checkCommands(t, k.commands(), onChannel0([]string{
			"CREATE_ID ps=0x0106 qp_type=2",
			"SET_OPTION id=1 level=0 name=3 len=1 value=14",
			"DESTROY_ID id=1",
		}))
	})
	t.Run("ListenConfig", func(t *testing.T) {
		k := newFakeKernel(t)
		withDevice(t, fakeGUID, fakeIndex)
		k.failing(opSetOption, syscall.EINVAL)
		_, err := ListenConfig{ReuseAddr: true}.Listen(netip.MustParseAddrPort("0.0.0.0:0"), 4)
		checkOpError(t, err, "rdma_set_option", syscall.EINVAL)
		checkCommands(t, k.commands(), onChannel0([]string{
			"CREATE_ID ps=0x0106 qp_type=2",
			"SET_OPTION id=1 level=0 name=1 len=4 value=1",
			"DESTROY_ID id=1",
		}))
	})
	t.Run("request", func(t *testing.T) {
		k, ln := listenOnFake(t, ListenConfig{AckTimeout: new(uint8(16))}.Listen)
		n := len(k.commands())
		k.answerQuery(fakeGUID, fakeIndex)
		k.failing(opSetOption, syscall.EINVAL)
		req, err := ln.request(requestEvent(ln, 9))
		if req != nil {
			t.Errorf("request() = %+v, want none", req)
		}
		checkOpError(t, err, "rdma_set_option", syscall.EINVAL)
		checkCommands(t, k.commands()[n:], []string{
			"ch0 QUERY id=9",
			"ch1 MIGRATE_ID id=9",
			"ch1 SET_OPTION id=9 level=0 name=3 len=1 value=16",
			"ch1 DESTROY_ID id=9",
		})
	})
}

// An ACK timeout above 31, which the 5 bits of the queue pair's attribute
// cannot hold, is refused before any command.
func TestAckTimeoutAbove31IsRefusedBeforeAnyCommand(t *testing.T) {
	for _, tt := range []struct {
		name string
		call func() error
	}{
		{"Dialer", func() error {
			_, err := Dialer{AckTimeout: new(uint8(32))}.Resolve(context.Background(), netip.AddrPort{}, fakeRemote)
			return err
		}},
		{"ListenConfig", func() error {
			_, err := ListenConfig{AckTimeout: new(uint8(255))}.Listen(netip.MustParseAddrPort("0.0.0.0:0"), 4)
			return err
		}},
	} {
		t.Run(tt.name, func(t *testing.T) {
			k := newFakeKernel(t)
			withDevice(t, fakeGUID, fakeIndex)
			// Without the check Resolve fails here instead of awaiting an event.
			k.failing(opResolveAddr, syscall.ENETUNREACH)
			err := tt.call()
			var opErr *verbs.OpError
			if !errors.As(err, &opErr) || opErr.Op != "rdma_set_option" || !errors.Is(err, syscall.EINVAL) {
				t.Errorf("error %#v, want a *verbs.OpError of rdma_set_option matching EINVAL", err)
			}
			checkCommands(t, k.commands(), nil)
		})
	}
}
