//go:build linux && !android && (amd64 || arm64 || loong64 || ppc64le || riscv64 || s390x)

package cm_test

import (
	"context"
	"net/netip"
	"strings"
	"syscall"
	"testing"
	"time"

	"github.com/wzshiming/rdma/internal/rdmatest"
	"github.com/wzshiming/rdma/internal/ucma"
	"github.com/wzshiming/rdma/verbs"
)

// peerECE is the ECE of a peer whose queue pair has ECE.
var peerECE = ucma.ECE{VendorID: 0x0002c9, AttrMod: 0x5}

// peerParam is what the peer's CONNECT and ACCEPT carry; it has no queue
// pair, so the number is made up.
var peerParam = ucma.ConnParam{QPNum: 0xabcde, ResponderResources: 1, InitiatorDepth: 1, RetryCount: 7, RNRRetryCount: 7}

// eceDevice returns the rdmatest device if it is rxe, whose queue pairs have
// no ECE while its connection manager carries the peer's.
func eceDevice(t *testing.T) rdmatest.Dev {
	t.Helper()
	dev := rdmatest.Device(t)
	if !strings.HasPrefix(dev.Name, "rxe") {
		t.Skipf("%s is not rxe, whose queue pairs have no ECE", dev.Name)
	}
	return dev
}

// peerChannel opens a channel of the kernel's rdma_cm device with an
// identifier of uid on it, for a peer the test drives command by command.
func peerChannel(t *testing.T, uid uint64) (*ucma.Channel, uint32) {
	t.Helper()
	ch, err := ucma.Open()
	if err != nil {
		t.Fatalf("ucma.Open() error: %v", err)
	}
	t.Cleanup(func() { ch.Close() })
	id, err := ch.CreateID(uid, ucma.PortSpaceTCP, ucma.QPTypeRC)
	if err != nil {
		t.Fatalf("CreateID() error: %v", err)
	}
	return ch, id
}

// peerEvent fetches the next event of the peer's channel, which must be of
// type want.
func peerEvent(t *testing.T, ch *ucma.Channel, want ucma.EventType) ucma.Event {
	t.Helper()
	ctx, cancel := context.WithTimeout(context.Background(), timeout)
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

// TestAcceptReturnsTheVendorOfTheRequest has a peer connect to a listener of
// each implementation on rxe with ECE in its CONNECT, or none: the ACCEPT
// returns the vendor of the request, without options.
func TestAcceptReturnsTheVendorOfTheRequest(t *testing.T) {
	dev := eceDevice(t)
	forEachImpl(t, func(t *testing.T, im impl) {
		for _, tt := range []struct {
			name            string
			request, accept ucma.ECE
		}{
			{"with ECE", peerECE, ucma.ECE{VendorID: peerECE.VendorID}},
			{"without ECE", ucma.ECE{}, ucma.ECE{}},
		} {
			t.Run(tt.name, func(t *testing.T) {
				ln := listenOn(t, im, netip.AddrPortFrom(dev.IP, 0))
				ch, id := peerChannel(t, 1)
				if err := ch.ResolveAddr(id, netip.AddrPortFrom(dev.IP, 0), ln.Addr(), 2*time.Second); err != nil {
					t.Fatalf("ResolveAddr() error: %v", err)
				}
				peerEvent(t, ch, ucma.EventAddrResolved)
				if err := ch.ResolveRoute(id, 2*time.Second); err != nil {
					t.Fatalf("ResolveRoute() error: %v", err)
				}
				peerEvent(t, ch, ucma.EventRouteResolved)
				if err := ch.Connect(id, peerParam, tt.request); err != nil {
					t.Fatalf("the peer's Connect() error: %v", err)
				}
				req := acceptRequest(t, ln)
				createQP(t, req.ID)
				accepted := make(chan error, 1)
				go func() {
					ctx, cancel := context.WithTimeout(context.Background(), timeout)
					defer cancel()
					accepted <- req.Accept(ctx, nil)
				}()
				if ev := peerEvent(t, ch, ucma.EventConnectResponse); ev.ECE != tt.accept {
					t.Errorf("ECE of the ACCEPT = %+v, want %+v", ev.ECE, tt.accept)
				}
				if err := ch.Accept(id, 0, nil, ucma.ECE{}); err != nil {
					t.Fatalf("the peer's Accept() error: %v", err)
				}
				if err := <-accepted; err != nil {
					t.Errorf("Request.Accept() error: %v", err)
				}
			})
		}
	})
}

// TestConnectRefusesAnAnswerOfAnotherVendor connects an ID of each
// implementation on rxe to a peer: CONNECT carries no ECE, and an answer
// with the ECE of a vendor, which is not the queue pair's, fails Connect
// with ETIMEDOUT and leaves the queue pair in ERR, while an answer without
// ECE connects.
func TestConnectRefusesAnAnswerOfAnotherVendor(t *testing.T) {
	dev := eceDevice(t)
	forEachImpl(t, func(t *testing.T, im impl) {
		for _, tt := range []struct {
			name    string
			answer  ucma.ECE
			refused bool
		}{
			{"of a vendor", peerECE, true},
			{"without ECE", ucma.ECE{AttrMod: peerECE.AttrMod}, false},
		} {
			t.Run(tt.name, func(t *testing.T) {
				ch, lid := peerChannel(t, 1)
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
				client := resolve(t, im, dev, bound.Src)
				res := connectAsync(client, nil)
				req := peerEvent(t, ch, ucma.EventConnectRequest)
				if req.ECE != (ucma.ECE{}) {
					t.Errorf("ECE of the CONNECT = %+v, want none", req.ECE)
				}
				if err := ch.Accept(req.ID, 2, &peerParam, tt.answer); err != nil {
					t.Fatalf("the peer's Accept() error: %v", err)
				}
				r := <-res
				want := verbs.QPStateRTS
				if tt.refused {
					want = verbs.QPStateErr
					if !isOpError(r.err, "rdma_connect", syscall.ETIMEDOUT) {
						t.Errorf("Connect() = %v, %v; want a *verbs.OpError of rdma_connect with ETIMEDOUT", r.info, r.err)
					}
				} else if r.err != nil {
					t.Errorf("Connect() error: %v", r.err)
				}
				if s := qpState(t, client.QP()); s != want {
					t.Errorf("client QP state = %v, want %v", s, want)
				}
			})
		}
	})
}
