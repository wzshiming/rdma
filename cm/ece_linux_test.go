//go:build linux && !android && (amd64 || arm64 || loong64 || ppc64le || riscv64 || s390x)

package cm

import (
	"context"
	"net/netip"
	"reflect"
	"strings"
	"syscall"
	"testing"
	"time"

	"github.com/wzshiming/rdma/internal/rdmatest"
	"github.com/wzshiming/rdma/internal/ucma"
	"github.com/wzshiming/rdma/verbs"
)

// Two vendors as ECE names them, by IEEE OUI.
const vendor, otherVendor = 0x0002c9, 0x00e04c

// notSupported is what verbs reports for a provider or a libibverbs without
// ECE.
var notSupported = &verbs.OpError{Op: "ibv_query_ece", Err: syscall.EOPNOTSUPP}

// eceProvider is the ECE side of a queue pair: QueryECE reports ece, whose
// options SetECE replaces, as a provider that accepts what is set; every
// SetECE is recorded. queryErr and setErr fail the calls instead.
type eceProvider struct {
	ece              verbs.ECE
	queryErr, setErr error
	set              []verbs.ECE
}

func (p *eceProvider) QueryECE() (verbs.ECE, error) {
	if p.queryErr != nil {
		return verbs.ECE{}, p.queryErr
	}
	return p.ece, nil
}

func (p *eceProvider) SetECE(e verbs.ECE) error {
	p.set = append(p.set, e)
	if p.setErr != nil {
		return p.setErr
	}
	p.ece.Options = e.Options
	return nil
}

// withoutECE is a queue pair whose provider has no ECE.
func withoutECE() *eceProvider { return &eceProvider{queryErr: notSupported, setErr: notSupported} }

// createQPECE runs the ECE steps of rdma_create_qp on qp: init_ece, then,
// once the queue pair is in INIT, set_local_ece.
func createQPECE(t *testing.T, id *ID, qp eceQP) {
	t.Helper()
	if err := id.initECE(qp); err != nil {
		t.Fatalf("initECE() error: %v", err)
	}
	if err := id.setLocalECE(qp); err != nil {
		t.Fatalf("setLocalECE() error: %v", err)
	}
}

// TestECEOfARequest pins what rdma_create_qp sets on the queue pair of a
// request, as init_ece decides it from the ECE of the request, and what
// rdma_accept's ACCEPT carries: the request's vendor, returned as received,
// with the options the queue pair accepted.
func TestECEOfARequest(t *testing.T) {
	for _, tt := range []struct {
		name    string
		request ucma.ECE
		set     []verbs.ECE
		accept  ucma.ECE
	}{
		{"from the same vendor", ucma.ECE{VendorID: vendor, AttrMod: 0x5},
			[]verbs.ECE{{VendorID: vendor, Options: 0x5}}, ucma.ECE{VendorID: vendor, AttrMod: 0x5}},
		{"from another vendor", ucma.ECE{VendorID: otherVendor, AttrMod: 0x5},
			[]verbs.ECE{{VendorID: vendor}}, ucma.ECE{VendorID: otherVendor}},
		{"without ECE", ucma.ECE{}, nil, ucma.ECE{AttrMod: 0x7}},
	} {
		t.Run(tt.name, func(t *testing.T) {
			id := new(ID)
			id.receivedECE(tt.request)
			qp := &eceProvider{ece: verbs.ECE{VendorID: vendor, Options: 0x7}}
			createQPECE(t, id, qp)
			if !reflect.DeepEqual(qp.set, tt.set) {
				t.Errorf("set on the queue pair: %+v, want %+v", qp.set, tt.set)
			}
			if got := id.acceptECE(); got != tt.accept {
				t.Errorf("ECE of ACCEPT = %+v, want %+v", got, tt.accept)
			}
		})
	}
}

// TestECEOfConnect pins that rdma_create_qp sets nothing on the queue pair
// of an ID that connects, and that CONNECT carries the queue pair's ECE as
// init_ece queried it: set_local_ece, which would query it again in INIT,
// skips an ID that connects.
func TestECEOfConnect(t *testing.T) {
	id := new(ID)
	qp := &eceProvider{ece: verbs.ECE{VendorID: vendor, Options: 0x7}}
	if err := id.initECE(qp); err != nil {
		t.Fatalf("initECE() error: %v", err)
	}
	qp.ece.Options = 0x3
	if err := id.setLocalECE(qp); err != nil {
		t.Fatalf("setLocalECE() error: %v", err)
	}
	if qp.set != nil {
		t.Errorf("set on the queue pair: %+v, want nothing", qp.set)
	}
	if got, want := id.connectECE(), (ucma.ECE{VendorID: vendor, AttrMod: 0x7}); got != want {
		t.Errorf("ECE of CONNECT = %+v, want %+v", got, want)
	}
}

// TestECEOfAConnectResponse pins what ucma_process_conn_resp_ece sets on
// the queue pair for the peer's answer to CONNECT: the answer of the same
// vendor, the local vendor without options for an answer without ECE, and
// EINVAL for an answer of another vendor, which keeps the peer's vendor
// unknown. The options the queue pair then accepts become the local ones.
func TestECEOfAConnectResponse(t *testing.T) {
	for _, tt := range []struct {
		name   string
		answer ucma.ECE
		set    verbs.ECE
		err    error
		remote uint32
	}{
		{"from the same vendor", ucma.ECE{VendorID: vendor, AttrMod: 0x5}, verbs.ECE{VendorID: vendor, Options: 0x5}, nil, vendor},
		{"without ECE", ucma.ECE{AttrMod: 0x5}, verbs.ECE{VendorID: vendor}, nil, 0},
		{"from another vendor", ucma.ECE{VendorID: otherVendor, AttrMod: 0x5}, verbs.ECE{}, syscall.EINVAL, 0},
	} {
		t.Run(tt.name, func(t *testing.T) {
			id := new(ID)
			qp := &eceProvider{ece: verbs.ECE{VendorID: vendor, Options: 0x7}}
			createQPECE(t, id, qp)
			set, err := id.answerECE(tt.answer)
			if set != tt.set || err != tt.err {
				t.Fatalf("answerECE(%+v) = %+v, %v; want %+v, %v", tt.answer, set, err, tt.set, tt.err)
			}
			if id.remoteECE.VendorID != tt.remote {
				t.Errorf("the peer's vendor = %#x, want %#x", id.remoteECE.VendorID, tt.remote)
			}
			if err != nil {
				return
			}
			qp.SetECE(set)
			if err := id.queryLocalECE(qp); err != nil {
				t.Fatalf("queryLocalECE() error: %v", err)
			}
			if want := (verbs.ECE{VendorID: vendor, Options: set.Options}); id.localECE != want {
				t.Errorf("local ECE after the response = %+v, want %+v", id.localECE, want)
			}
		})
	}
}

// TestECENotSupportedChangesNothing runs every ECE step on a queue pair
// whose provider, or libibverbs, has no ECE: no step fails, CONNECT carries
// no ECE, ACCEPT only the vendor of the request, returned as received, and
// a connect response without ECE goes ahead.
func TestECENotSupportedChangesNothing(t *testing.T) {
	t.Run("connect", func(t *testing.T) {
		id := new(ID)
		qp := withoutECE()
		createQPECE(t, id, qp)
		if got := id.connectECE(); got != (ucma.ECE{}) {
			t.Errorf("ECE of CONNECT = %+v, want none", got)
		}
		set, err := id.answerECE(ucma.ECE{})
		if err != nil {
			t.Fatalf("answerECE(none) error: %v", err)
		}
		if err := eceError(qp.SetECE(set)); err != nil {
			t.Errorf("setting %+v: %v, want no error", set, err)
		}
		if err := id.queryLocalECE(qp); err != nil || id.localECE != (verbs.ECE{}) {
			t.Errorf("queryLocalECE() = %v with the local ECE %+v; want no error and none", err, id.localECE)
		}
	})
	for _, tt := range []struct {
		name    string
		request ucma.ECE
	}{
		{"request with ECE", ucma.ECE{VendorID: vendor, AttrMod: 0x5}},
		{"request without ECE", ucma.ECE{}},
	} {
		t.Run(tt.name, func(t *testing.T) {
			id := new(ID)
			id.receivedECE(tt.request)
			createQPECE(t, id, withoutECE())
			if want := (ucma.ECE{VendorID: tt.request.VendorID}); id.acceptECE() != want {
				t.Errorf("ECE of ACCEPT = %+v, want %+v", id.acceptECE(), want)
			}
		})
	}
}

// TestECEErrorFailsItsStep pins that an ECE call failing otherwise than
// with EOPNOTSUPP fails its step with its errno, as it fails librdmacm's.
func TestECEErrorFailsItsStep(t *testing.T) {
	failing := &verbs.OpError{Op: "ibv_query_ece", Err: syscall.EIO}
	request := ucma.ECE{VendorID: vendor, AttrMod: 0x5}
	for _, tt := range []struct {
		name string
		step func(id *ID) error
	}{
		{"init_ece, query", func(id *ID) error { return id.initECE(&eceProvider{queryErr: failing}) }},
		{"init_ece, set", func(id *ID) error {
			id.receivedECE(request)
			return id.initECE(&eceProvider{ece: verbs.ECE{VendorID: vendor}, setErr: failing})
		}},
		{"set_local_ece", func(id *ID) error {
			id.receivedECE(request)
			return id.setLocalECE(&eceProvider{queryErr: failing})
		}},
		{"query after the response", func(id *ID) error { return id.queryLocalECE(&eceProvider{queryErr: failing}) }},
	} {
		if err := tt.step(new(ID)); err != syscall.EIO {
			t.Errorf("%s: error %v, want EIO", tt.name, err)
		}
	}
}

// TestConnectResponseWithoutQPKeepsThePeersECE pins the connect response
// that finds no queue pair: rdma_get_cm_event hands it out as it is and
// keeps its ECE as the peer's.
func TestConnectResponseWithoutQPKeepsThePeersECE(t *testing.T) {
	id := newID(nil, 3, 7)
	ev, err := id.process(ucma.Event{Type: ucma.EventConnectResponse, UID: 7, ID: 3, ECE: ucma.ECE{VendorID: vendor, AttrMod: 0x5}}, "rdma_connect")
	if err != nil || ev.typ != EventConnectResponse {
		t.Fatalf("process(CONNECT_RESPONSE) = %+v, %v; want CONNECT_RESPONSE", ev, err)
	}
	if want := (verbs.ECE{VendorID: vendor, Options: 0x5}); id.remoteECE != want {
		t.Errorf("the peer's ECE = %+v, want %+v", id.remoteECE, want)
	}
}

// useECE gives every queue pair the ECE of p until the test ends.
func useECE(t *testing.T, p *eceProvider) {
	old := eceOf
	eceOf = func(*verbs.QP) eceQP { return p }
	t.Cleanup(func() { eceOf = old })
}

// onRXE returns the rdmatest device if it is rxe, whose connection manager
// carries the ECE of CONNECT and ACCEPT.
func onRXE(t *testing.T) rdmatest.Dev {
	t.Helper()
	dev := rdmatest.Device(t)
	if !strings.HasPrefix(dev.Name, "rxe") {
		t.Skipf("%s is not rxe", dev.Name)
	}
	return dev
}

// peerParam is what the peer's CONNECT and ACCEPT carry; it has no queue
// pair, so the number is made up.
var peerParam = ucma.ConnParam{QPNum: 0xabcde, ResponderResources: 1, InitiatorDepth: 1, RetryCount: 7, RNRRetryCount: 7}

// peerChannel opens a channel of the kernel's rdma_cm device with an
// identifier on it, for a peer the test drives command by command.
func peerChannel(t *testing.T) (*ucma.Channel, uint32) {
	t.Helper()
	ch, err := ucma.Open()
	if err != nil {
		t.Fatalf("ucma.Open() error: %v", err)
	}
	t.Cleanup(func() { ch.Close() })
	id, err := ch.CreateID(1, ucma.PortSpaceTCP, ucma.QPTypeRC)
	if err != nil {
		t.Fatalf("CreateID() error: %v", err)
	}
	return ch, id
}

// peerEvent fetches the next event of the peer's channel, which must be of
// type want.
func peerEvent(t *testing.T, ch *ucma.Channel, want ucma.EventType) ucma.Event {
	t.Helper()
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()
	ev, err := ch.GetEvent(ctx)
	if err != nil {
		t.Fatalf("the peer's GetEvent() error: %v", err)
	}
	if ev.Type != want {
		t.Fatalf("the peer got %v with status %d, want %v", ev.Type, ev.Status, want)
	}
	return ev
}

// withQP gives id a queue pair on the device's default protection domain;
// the ID closes before the completion queue when the test ends.
func withQP(t *testing.T, id *ID) {
	t.Helper()
	cq, err := id.Device().CreateCQ(4, nil)
	if err != nil {
		t.Fatalf("CreateCQ() error: %v", err)
	}
	t.Cleanup(func() {
		id.Close()
		cq.Close()
	})
	attr := verbs.QPInitAttr{SendCQ: cq, RecvCQ: cq, Type: verbs.QPTypeRC, Cap: verbs.QPCap{MaxSendWR: 1, MaxRecvWR: 1, MaxSendSGE: 1, MaxRecvSGE: 1}}
	if _, err := id.CreateQP(nil, &attr); err != nil {
		t.Fatalf("CreateQP() error: %v", err)
	}
}

// TestConnectExchangesTheQueuePairsECE connects an ID whose queue pair has
// ECE to a peer on rxe that answers with other options: CONNECT carries the
// queue pair's ECE, the answer is set on the queue pair, and the options it
// accepted become the local ones.
func TestConnectExchangesTheQueuePairsECE(t *testing.T) {
	dev := onRXE(t)
	qp := &eceProvider{ece: verbs.ECE{VendorID: vendor, Options: 0x7}}
	useECE(t, qp)
	ch, lid := peerChannel(t)
	if err := ch.Bind(lid, netip.AddrPortFrom(dev.IP, 0)); err != nil {
		t.Fatalf("Bind() error: %v", err)
	}
	if err := ch.Listen(lid, 1); err != nil {
		t.Fatalf("Listen() error: %v", err)
	}
	bound, err := ch.QueryAddr(lid)
	if err != nil {
		t.Fatalf("QueryAddr() error: %v", err)
	}
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()
	id, err := Resolve(ctx, netip.AddrPortFrom(dev.IP, 0), bound.Src)
	if err != nil {
		t.Fatalf("Resolve() error: %v", err)
	}
	withQP(t, id)
	connected := make(chan error, 1)
	go func() {
		_, err := id.Connect(ctx, nil)
		connected <- err
	}()
	req := peerEvent(t, ch, ucma.EventConnectRequest)
	if want := (ucma.ECE{VendorID: vendor, AttrMod: 0x7}); req.ECE != want {
		t.Errorf("ECE of CONNECT = %+v, want %+v", req.ECE, want)
	}
	if err := ch.Accept(req.ID, 2, &peerParam, ucma.ECE{VendorID: vendor, AttrMod: 0x5}); err != nil {
		t.Fatalf("the peer's Accept() error: %v", err)
	}
	if err := <-connected; err != nil {
		t.Fatalf("Connect() error: %v", err)
	}
	if want := []verbs.ECE{{VendorID: vendor, Options: 0x5}}; !reflect.DeepEqual(qp.set, want) {
		t.Errorf("set on the queue pair: %+v, want %+v", qp.set, want)
	}
	if want := (verbs.ECE{VendorID: vendor, Options: 0x5}); id.localECE != want {
		t.Errorf("local ECE after the response = %+v, want %+v", id.localECE, want)
	}
}

// TestAcceptExchangesTheQueuePairsECE accepts the request of a peer on rxe
// with ECE of the queue pair's vendor: CreateQP sets the request's ECE on the
// queue pair, and ACCEPT carries the options the queue pair accepted.
func TestAcceptExchangesTheQueuePairsECE(t *testing.T) {
	dev := onRXE(t)
	qp := &eceProvider{ece: verbs.ECE{VendorID: vendor, Options: 0x7}}
	useECE(t, qp)
	ln, err := Listen(netip.AddrPortFrom(dev.IP, 0), 1)
	if err != nil {
		t.Fatalf("Listen() error: %v", err)
	}
	t.Cleanup(func() { ln.Close() })
	ch, pid := peerChannel(t)
	if err := ch.ResolveAddr(pid, netip.AddrPortFrom(dev.IP, 0), ln.Addr(), 2*time.Second); err != nil {
		t.Fatalf("ResolveAddr() error: %v", err)
	}
	peerEvent(t, ch, ucma.EventAddrResolved)
	if err := ch.ResolveRoute(pid, 2*time.Second); err != nil {
		t.Fatalf("ResolveRoute() error: %v", err)
	}
	peerEvent(t, ch, ucma.EventRouteResolved)
	if err := ch.Connect(pid, peerParam, ucma.ECE{VendorID: vendor, AttrMod: 0x5}); err != nil {
		t.Fatalf("the peer's Connect() error: %v", err)
	}
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()
	req, err := ln.Accept(ctx)
	if err != nil {
		t.Fatalf("Accept() error: %v", err)
	}
	withQP(t, req.ID)
	if want := []verbs.ECE{{VendorID: vendor, Options: 0x5}}; !reflect.DeepEqual(qp.set, want) {
		t.Errorf("set on the queue pair: %+v, want %+v", qp.set, want)
	}
	accepted := make(chan error, 1)
	go func() { accepted <- req.Accept(ctx, nil) }()
	if ev, want := peerEvent(t, ch, ucma.EventConnectResponse), (ucma.ECE{VendorID: vendor, AttrMod: 0x5}); ev.ECE != want {
		t.Errorf("ECE of ACCEPT = %+v, want %+v", ev.ECE, want)
	}
	if err := ch.Accept(pid, 0, nil, ucma.ECE{}); err != nil {
		t.Fatalf("the peer's Accept() error: %v", err)
	}
	if err := <-accepted; err != nil {
		t.Errorf("Request.Accept() error: %v", err)
	}
}
