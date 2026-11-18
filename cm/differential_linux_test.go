//go:build linux && !android && (amd64 || arm64 || loong64 || ppc64le || riscv64 || s390x)

package cm_test

import (
	"context"
	"errors"
	"fmt"
	"net/netip"
	"reflect"
	"strconv"
	"strings"
	"syscall"
	"testing"

	"github.com/wzshiming/rdma/cm"
	"github.com/wzshiming/rdma/internal/rdmatest"
	"github.com/wzshiming/rdma/verbs"
)

// TestSameAsReference makes the same connections with the implementation
// under test on both ends and with the reference on both ends, and compares
// what the two leave behind: what each end learns from the other, the queue
// pair attributes the connection manager sets on either end, the queue pair
// states after Disconnect and after a failed Connect, and the errors of a
// rejected and a refused Connect. A scenario with options sets every option
// on both ends.
func TestSameAsReference(t *testing.T) {
	dev := testDevice(t)
	for _, tt := range []struct {
		name string
		scenario
	}{
		{"default parameters", scenario{}},
		{"non-default parameters", scenario{
			connect: &cm.ConnParam{PrivateData: []byte("client hello"), ResponderResources: 2, InitiatorDepth: 3, RetryCount: 4, RNRRetryCount: 5},
			accept:  &cm.ConnParam{PrivateData: []byte("server welcome"), ResponderResources: 3, InitiatorDepth: 2, RetryCount: 1, RNRRetryCount: 6},
		}},
		{"options", scenario{
			dialer:       cm.Dialer{TOS: new(uint8(0x60)), ReuseAddr: true, AFOnly: new(true), AckTimeout: new(uint8(14))},
			listenConfig: cm.ListenConfig{TOS: new(uint8(0x20)), ReuseAddr: true, AFOnly: new(true), AckTimeout: new(uint8(16))},
		}},
	} {
		t.Run(tt.name, func(t *testing.T) {
			compare(t, dev, func(t *testing.T, im impl, at endpoints) connection {
				return connectAt(t, im, at, tt.scenario)
			})
		})
	}
	t.Run("rejected", func(t *testing.T) { compare(t, dev, rejectAt) })
	t.Run("refused", func(t *testing.T) { compare(t, dev, refuseAt) })
}

// endpoints are the addresses of a connection's two ends.
type endpoints struct{ listener, client netip.AddrPort }

// compare runs record on the implementation under test and then on the
// reference, each in a subtest named after it and both on the same
// endpoints, so that what the kernel derives from the ports is the same too,
// and reports every field in which the two records differ.
func compare[R any](t *testing.T, dev rdmatest.Dev, record func(t *testing.T, im impl, at endpoints) R) {
	t.Helper()
	at := freeEndpoints(t, dev)
	var records [2]R
	for i, im := range []impl{underTest, reference} {
		if !t.Run(im.name, func(t *testing.T) { records[i] = record(t, im, at) }) {
			return
		}
	}
	for _, d := range differences("", reflect.ValueOf(records[0]), reflect.ValueOf(records[1])) {
		t.Errorf("%s: %s %s, %s %s", d.path, underTest.name, d.got, reference.name, d.want)
	}
}

// freeEndpoints returns two addresses on the device's IP whose ports are
// free in rdma_cm's port space: the reference listens on port 0 twice, then
// closes both listeners.
func freeEndpoints(t *testing.T, dev rdmatest.Dev) endpoints {
	t.Helper()
	var lns [2]listener
	var addrs [2]netip.AddrPort
	for i := range lns {
		lns[i] = listenOn(t, reference, netip.AddrPortFrom(dev.IP, 0))
		addrs[i] = lns[i].Addr()
	}
	for _, ln := range lns {
		if err := ln.Close(); err != nil {
			t.Fatalf("Listener.Close() error: %v", err)
		}
	}
	return endpoints{listener: addrs[0], client: addrs[1]}
}

// connection is what a connection leaves behind: what each end learned from
// the other, both ends as the connection manager set them up, the server's
// queue pair state once its Wait saw the client disconnect, and the states
// of both once both disconnected.
type connection struct {
	Request             received
	ConnInfo            received
	Client, Server      end
	ServerSawDisconnect verbs.QPState
	Disconnected        struct{ Client, Server verbs.QPState }
}

// received is what one end learned from the other: the private data and the
// parameters of a Request or a ConnInfo.
type received struct {
	PrivateData        string
	ResponderResources uint8
	InitiatorDepth     uint8
}

// end is one end of a connection: its addresses, its port and the attributes
// a connection manager sets on its queue pair.
type end struct {
	Local, Remote netip.AddrPort
	Port          uint8
	QP            qpAttrs
}

type qpAttrs struct {
	State           verbs.QPState
	PathMTU         verbs.MTU
	Timeout         uint8
	RetryCnt        uint8
	RNRRetry        uint8
	MaxRDAtomic     uint8
	MaxDestRDAtomic uint8
	MinRNRTimer     uint8
	PkeyIndex       uint16
	Port            uint8
	AccessFlags     verbs.AccessFlags
	AH              verbs.AHAttr
}

const cmAttrs = verbs.QPAttrState | verbs.QPAttrPathMTU | verbs.QPAttrTimeout | verbs.QPAttrRetryCnt |
	verbs.QPAttrRNRRetry | verbs.QPAttrMaxQPRDAtomic | verbs.QPAttrMaxDestRDAtomic | verbs.QPAttrMinRNRTimer |
	verbs.QPAttrPkeyIndex | verbs.QPAttrPort | verbs.QPAttrAccessFlags | verbs.QPAttrAV

func endOf(t *testing.T, id conn) end {
	t.Helper()
	a, err := id.QP().Query(cmAttrs)
	if err != nil {
		t.Fatalf("QP.Query() error: %v", err)
	}
	return end{
		Local:  id.LocalAddr(),
		Remote: id.RemoteAddr(),
		Port:   id.Port(),
		QP: qpAttrs{
			State: a.State, PathMTU: a.PathMTU, Timeout: a.Timeout, RetryCnt: a.RetryCnt, RNRRetry: a.RNRRetry,
			MaxRDAtomic: a.MaxRDAtomic, MaxDestRDAtomic: a.MaxDestRDAtomic, MinRNRTimer: a.MinRNRTimer,
			PkeyIndex: a.PkeyIndex, Port: a.Port, AccessFlags: a.AccessFlags, AH: a.AH,
		},
	}
}

// scenario is how connectAt connects: with the options of the client's and
// the listener's identifiers, and the parameters of Connect and Accept.
type scenario struct {
	dialer          cm.Dialer
	listenConfig    cm.ListenConfig
	connect, accept *cm.ConnParam
}

// connectAt connects a client of im from at.client to a listener of im on
// at.listener as s says, then disconnects both ends.
func connectAt(t *testing.T, im impl, at endpoints, s scenario) connection {
	t.Helper()
	ln := listenWith(t, im, s.listenConfig, at.listener)
	client := resolveWith(t, im, s.dialer, at.client, at.listener)
	ctx, cancel := context.WithTimeout(context.Background(), timeout)
	defer cancel()
	res := make(chan connectResult, 1)
	go func() {
		info, err := client.Connect(ctx, s.connect)
		res <- connectResult{info, err}
	}()
	req := acceptRequest(t, ln)
	createQP(t, req.ID)
	if err := req.Accept(ctx, s.accept); err != nil {
		t.Fatalf("Request.Accept() error: %v", err)
	}
	r := <-res
	if r.err != nil {
		t.Fatalf("Connect() error: %v", r.err)
	}
	c := connection{
		Request:  received{string(req.PrivateData), req.ResponderResources, req.InitiatorDepth},
		ConnInfo: received{string(r.info.PrivateData), r.info.ResponderResources, r.info.InitiatorDepth},
		Client:   endOf(t, client),
		Server:   endOf(t, req.ID),
	}

	errc := make(chan error, 1)
	go func() { errc <- client.Disconnect(ctx) }()
	if ev, err := req.ID.Wait(ctx); err != nil || ev != cm.EventDisconnected {
		t.Fatalf("Wait() = %v, %v; want %v", ev, err, cm.EventDisconnected)
	}
	c.ServerSawDisconnect = qpState(t, req.ID.QP())
	if err := req.ID.Disconnect(ctx); err != nil {
		t.Fatalf("server Disconnect() error: %v", err)
	}
	if err := <-errc; err != nil {
		t.Fatalf("client Disconnect() error: %v", err)
	}
	c.Disconnected.Client = qpState(t, client.QP())
	c.Disconnected.Server = qpState(t, req.ID.QP())
	return c
}

// failure is what a client is left with after a failed Connect: the error
// and the state of its queue pair.
type failure struct {
	Err     errorView
	QPState verbs.QPState
}

// rejectAt connects a client of im from at.client to a listener of im on
// at.listener, which rejects the request.
func rejectAt(t *testing.T, im impl, at endpoints) failure {
	t.Helper()
	ln := listenOn(t, im, at.listener)
	client := resolveFrom(t, im, at.client, at.listener)
	res := connectAsync(client, []byte("may I"))
	req := acceptRequest(t, ln)
	if err := req.Reject([]byte("no way")); err != nil {
		t.Fatalf("Reject() error: %v", err)
	}
	r := <-res
	if r.err == nil {
		t.Fatal("Connect() succeeded, want the request rejected")
	}
	return failure{viewOf(r.err), qpState(t, client.QP())}
}

// refuseAt connects a client of im from at.client to at.listener, where
// nobody listens.
func refuseAt(t *testing.T, im impl, at endpoints) failure {
	t.Helper()
	client := resolveFrom(t, im, at.client, at.listener)
	ctx, cancel := context.WithTimeout(context.Background(), timeout)
	defer cancel()
	_, err := client.Connect(ctx, nil)
	if err == nil {
		t.Fatalf("Connect(%v) succeeded, want it refused", at.listener)
	}
	return failure{viewOf(err), qpState(t, client.QP())}
}

// errorView is what a caller can tell an error by: its text, the types along
// its chain, whether it matches syscall.ECONNREFUSED, the operation of the
// *verbs.OpError and the fields of the *cm.RejectedError in it.
type errorView struct {
	Text        string
	Types       string
	Refused     bool
	Op          string
	Rejected    bool
	PrivateData string
	Reason      int
}

func viewOf(err error) errorView {
	var types []string
	for e := err; e != nil; e = errors.Unwrap(e) {
		types = append(types, fmt.Sprintf("%T", e))
	}
	v := errorView{Text: err.Error(), Types: strings.Join(types, " > "), Refused: errors.Is(err, syscall.ECONNREFUSED)}
	var opErr *verbs.OpError
	if errors.As(err, &opErr) {
		v.Op = opErr.Op
	}
	var rej *cm.RejectedError
	if errors.As(err, &rej) {
		v.Rejected, v.PrivateData, v.Reason = true, string(rej.PrivateData), rej.Reason
	}
	return v
}

// difference is a field in which two records differ, by its path, with its
// value in the record of the implementation under test and in that of the
// reference.
type difference struct{ path, got, want string }

var stringer = reflect.TypeFor[fmt.Stringer]()

// differences lists the fields in which a and b, values of one type, differ,
// looking into nested structs unless they print themselves.
func differences(path string, a, b reflect.Value) []difference {
	if a.Kind() == reflect.Struct && !a.Type().Implements(stringer) {
		var ds []difference
		for i := range a.NumField() {
			name := a.Type().Field(i).Name
			if path != "" {
				name = path + "." + name
			}
			ds = append(ds, differences(name, a.Field(i), b.Field(i))...)
		}
		return ds
	}
	if a.Equal(b) {
		return nil
	}
	return []difference{{path, show(a), show(b)}}
}

func show(v reflect.Value) string {
	if v.Kind() == reflect.String {
		return strconv.Quote(v.String())
	}
	return fmt.Sprint(v)
}
