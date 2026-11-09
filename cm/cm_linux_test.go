//go:build linux && cgo

package cm_test

import (
	"bytes"
	"context"
	"errors"
	"net"
	"net/netip"
	"os"
	"runtime"
	"strconv"
	"strings"
	"sync"
	"syscall"
	"testing"
	"time"

	"github.com/wzshiming/rdma/cm"
	"github.com/wzshiming/rdma/internal/rdmatest"
	"github.com/wzshiming/rdma/verbs"
)

const timeout = 10 * time.Second

// listen starts a Listener on port 0 of the rdmatest device's IP.
func listen(t *testing.T) (rdmatest.Dev, *cm.Listener) {
	t.Helper()
	dev := rdmatest.Device(t)
	ln, err := cm.Listen(netip.AddrPortFrom(dev.IP, 0), 8)
	if err != nil {
		t.Fatalf("Listen(%v:0) error: %v", dev.IP, err)
	}
	t.Cleanup(func() {
		if err := ln.Close(); err != nil {
			t.Errorf("Listener.Close() error: %v", err)
		}
	})
	return dev, ln
}

// resolve resolves remote from the device's IP and gives the ID a queue pair.
func resolve(t *testing.T, dev rdmatest.Dev, remote netip.AddrPort) *cm.ID {
	t.Helper()
	ctx, cancel := context.WithTimeout(context.Background(), timeout)
	defer cancel()
	id, err := cm.Resolve(ctx, netip.AddrPortFrom(dev.IP, 0), remote)
	if err != nil {
		t.Fatalf("Resolve(%v, %v) error: %v", dev.IP, remote, err)
	}
	createQP(t, id)
	return id
}

// createQP gives id an RC queue pair on a protection domain of its device
// and arranges for the ID to close before the completion queue and domain.
func createQP(t *testing.T, id *cm.ID) {
	t.Helper()
	dev := id.Device()
	if dev == nil {
		t.Fatal("Device() = nil")
	}
	pd, err := dev.AllocPD()
	if err != nil {
		t.Fatalf("AllocPD() error: %v", err)
	}
	cq, err := dev.CreateCQ(16, nil)
	if err != nil {
		pd.Close()
		t.Fatalf("CreateCQ() error: %v", err)
	}
	t.Cleanup(func() {
		if err := id.Close(); err != nil {
			t.Errorf("ID.Close() error: %v", err)
		}
		if err := cq.Close(); err != nil {
			t.Errorf("CQ.Close() error: %v", err)
		}
		if err := pd.Close(); err != nil {
			t.Errorf("PD.Close() error: %v", err)
		}
	})
	attr := verbs.QPInitAttr{
		SendCQ: cq,
		RecvCQ: cq,
		Type:   verbs.QPTypeRC,
		Cap:    verbs.QPCap{MaxSendWR: 4, MaxRecvWR: 4, MaxSendSGE: 1, MaxRecvSGE: 1},
	}
	qp, err := id.CreateQP(pd, &attr)
	if err != nil {
		t.Fatalf("CreateQP() error: %v", err)
	}
	if qp != id.QP() || qp.Num() == 0 || attr.Cap.MaxSendWR < 4 {
		t.Errorf("CreateQP() = QP %d (cap %+v), QP() = %p; want the same queue pair with the requested capacity", qp.Num(), attr.Cap, id.QP())
	}
}

type connectResult struct {
	info *cm.ConnInfo
	err  error
}

// connectAsync runs id.Connect with data in the background.
func connectAsync(id *cm.ID, data []byte) <-chan connectResult {
	res := make(chan connectResult, 1)
	go func() {
		ctx, cancel := context.WithTimeout(context.Background(), timeout)
		defer cancel()
		info, err := id.Connect(ctx, &cm.ConnParam{PrivateData: data})
		res <- connectResult{info, err}
	}()
	return res
}

// acceptRequest takes the next request of ln.
func acceptRequest(t *testing.T, ln *cm.Listener) *cm.Request {
	t.Helper()
	ctx, cancel := context.WithTimeout(context.Background(), timeout)
	defer cancel()
	req, err := ln.Accept(ctx)
	if err != nil {
		t.Fatalf("Accept() error: %v", err)
	}
	return req
}

// pair establishes a connection between a client ID and a request accepted
// from a Listener on the rdmatest device.
func pair(t *testing.T) (ln *cm.Listener, client *cm.ID, server *cm.Request) {
	t.Helper()
	dev, ln := listen(t)
	client = resolve(t, dev, ln.Addr())
	res := connectAsync(client, []byte("ping"))
	server = acceptRequest(t, ln)
	createQP(t, server.ID)
	ctx, cancel := context.WithTimeout(context.Background(), timeout)
	defer cancel()
	if err := server.Accept(ctx, &cm.ConnParam{PrivateData: []byte("pong")}); err != nil {
		t.Fatalf("Request.Accept() error: %v", err)
	}
	if r := <-res; r.err != nil {
		t.Fatalf("Connect() error: %v", r.err)
	}
	return ln, client, server
}

func qpState(t *testing.T, qp *verbs.QP) verbs.QPState {
	t.Helper()
	attr, err := qp.Query(verbs.QPAttrState)
	if err != nil {
		t.Fatalf("QP.Query() error: %v", err)
	}
	return attr.State
}

func TestListenPortZero(t *testing.T) {
	dev, ln := listen(t)
	addr := ln.Addr()
	if addr.Addr() != dev.IP || addr.Port() == 0 {
		t.Fatalf("Addr() = %v, want %v with a real port", addr, dev.IP)
	}
	// The port is taken: a second listener on it fails.
	ln2, err := cm.Listen(addr, 1)
	if err == nil {
		ln2.Close()
		t.Fatalf("second Listen(%v) succeeded, want EADDRINUSE", addr)
	}
	if !errors.Is(err, syscall.EADDRINUSE) {
		t.Errorf("second Listen(%v) error = %v, want EADDRINUSE", addr, err)
	}
}

func TestConnectAcceptExchangesPrivateData(t *testing.T) {
	dev, ln := listen(t)
	client := resolve(t, dev, ln.Addr())
	if client.Port() != uint8(dev.Port) || client.LocalAddr().Addr() != dev.IP || client.LocalAddr().Port() == 0 || client.RemoteAddr() != ln.Addr() {
		t.Errorf("resolved ID: port %d, local %v, remote %v; want port %d, %v with a port, %v", client.Port(), client.LocalAddr(), client.RemoteAddr(), dev.Port, dev.IP, ln.Addr())
	}

	res := connectAsync(client, []byte("client hello"))
	req := acceptRequest(t, ln)
	if !bytes.HasPrefix(req.PrivateData, []byte("client hello")) || len(req.PrivateData) != cm.MaxConnectPrivateData {
		t.Errorf("Request.PrivateData = %q (%d bytes), want %q padded to %d", req.PrivateData, len(req.PrivateData), "client hello", cm.MaxConnectPrivateData)
	}
	if req.ResponderResources != 1 || req.InitiatorDepth != 1 {
		t.Errorf("Request parameters = %d/%d, want the defaults 1/1", req.ResponderResources, req.InitiatorDepth)
	}
	if req.ID.LocalAddr() != ln.Addr() || req.ID.RemoteAddr() != client.LocalAddr() || req.ID.Port() != uint8(dev.Port) {
		t.Errorf("incoming ID: local %v, remote %v, port %d; want %v, %v, %d", req.ID.LocalAddr(), req.ID.RemoteAddr(), req.ID.Port(), ln.Addr(), client.LocalAddr(), dev.Port)
	}
	createQP(t, req.ID)
	ctx, cancel := context.WithTimeout(context.Background(), timeout)
	defer cancel()
	if err := req.Accept(ctx, &cm.ConnParam{PrivateData: []byte("server welcome")}); err != nil {
		t.Fatalf("Request.Accept() error: %v", err)
	}
	r := <-res
	if r.err != nil {
		t.Fatalf("Connect() error: %v", r.err)
	}
	if !bytes.HasPrefix(r.info.PrivateData, []byte("server welcome")) || len(r.info.PrivateData) != cm.MaxAcceptPrivateData {
		t.Errorf("ConnInfo.PrivateData = %q (%d bytes), want %q padded to %d", r.info.PrivateData, len(r.info.PrivateData), "server welcome", cm.MaxAcceptPrivateData)
	}
	if r.info.ResponderResources != 1 || r.info.InitiatorDepth != 1 {
		t.Errorf("ConnInfo parameters = %d/%d, want the defaults 1/1", r.info.ResponderResources, r.info.InitiatorDepth)
	}

	// Both queue pairs are ready to send and point at each other.
	a, err := client.QP().Query(verbs.QPAttrState | verbs.QPAttrDestQPN)
	if err != nil {
		t.Fatalf("client QP.Query() error: %v", err)
	}
	b, err := req.ID.QP().Query(verbs.QPAttrState | verbs.QPAttrDestQPN)
	if err != nil {
		t.Fatalf("server QP.Query() error: %v", err)
	}
	if a.State != verbs.QPStateRTS || b.State != verbs.QPStateRTS || a.DestQPNum != req.ID.QP().Num() || b.DestQPNum != client.QP().Num() {
		t.Errorf("queue pairs: client %v -> %d, server %v -> %d; want RTS -> %d and RTS -> %d", a.State, a.DestQPNum, b.State, b.DestQPNum, req.ID.QP().Num(), client.QP().Num())
	}

	if err := client.Close(); err != nil {
		t.Fatalf("Close() error: %v", err)
	}
	if err := client.Close(); err != nil {
		t.Errorf("second Close() error: %v", err)
	}
	if _, err := client.Connect(ctx, nil); !errors.Is(err, os.ErrClosed) {
		t.Errorf("Connect() after Close() error = %v, want os.ErrClosed", err)
	}
	if client.QP() != nil {
		t.Error("QP() after Close() is not nil")
	}
}

func TestRejectReturnsRejectedError(t *testing.T) {
	dev, ln := listen(t)
	client := resolve(t, dev, ln.Addr())
	res := connectAsync(client, []byte("may I"))
	req := acceptRequest(t, ln)
	if err := req.Reject([]byte("no way")); err != nil {
		t.Fatalf("Reject() error: %v", err)
	}
	r := <-res
	var rej *cm.RejectedError
	if !errors.As(r.err, &rej) {
		t.Fatalf("Connect() = %v, %v; want a *RejectedError", r.info, r.err)
	}
	if !bytes.HasPrefix(rej.PrivateData, []byte("no way")) || rej.Reason != 28 {
		t.Errorf("RejectedError = {%q, %d}, want data %q and reason 28", rej.PrivateData, rej.Reason, "no way")
	}
	if !errors.Is(r.err, syscall.ECONNREFUSED) {
		t.Errorf("errors.Is(err, ECONNREFUSED) = false, want true")
	}
	// Reject closed the request's ID.
	if _, err := req.ID.Wait(context.Background()); !errors.Is(err, os.ErrClosed) {
		t.Errorf("Wait() on the rejected ID error = %v, want os.ErrClosed", err)
	}
}

func TestDisconnectWakesPeerWait(t *testing.T) {
	_, client, server := pair(t)
	ctx, cancel := context.WithTimeout(context.Background(), timeout)
	defer cancel()
	errc := make(chan error, 1)
	go func() { errc <- client.Disconnect(ctx) }()
	ev, err := server.ID.Wait(ctx)
	if err != nil || ev != cm.EventDisconnected {
		t.Fatalf("Wait() = %v, %v; want %v", ev, err, cm.EventDisconnected)
	}
	// The passive side answers; having seen the peer's disconnect, it does
	// not wait for another one.
	start := time.Now()
	if err := server.ID.Disconnect(ctx); err != nil {
		t.Errorf("server Disconnect() error: %v", err)
	}
	if elapsed := time.Since(start); elapsed > 2*time.Second {
		t.Errorf("server Disconnect() took %v, want immediate", elapsed)
	}
	if err := <-errc; err != nil {
		t.Errorf("client Disconnect() error: %v", err)
	}
	if s := qpState(t, client.QP()); s != verbs.QPStateErr {
		t.Errorf("client QP state after Disconnect() = %v, want %v", s, verbs.QPStateErr)
	}
}

func TestConnectRefusedWithoutListener(t *testing.T) {
	dev, ln := listen(t)
	addr := ln.Addr()
	if err := ln.Close(); err != nil {
		t.Fatalf("Listener.Close() error: %v", err)
	}
	client := resolve(t, dev, addr)
	ctx, cancel := context.WithTimeout(context.Background(), timeout)
	defer cancel()
	info, err := client.Connect(ctx, nil)
	var opErr *verbs.OpError
	if !errors.As(err, &opErr) || !errors.Is(err, syscall.ECONNREFUSED) {
		t.Fatalf("Connect(%v) = %v, %v; want a *verbs.OpError with ECONNREFUSED", addr, info, err)
	}
	var rej *cm.RejectedError
	if errors.As(err, &rej) {
		t.Errorf("Connect(%v) error = %v, a port nobody listens on is not an explicit reject", addr, err)
	}
}

func TestAcceptCancelThenClose(t *testing.T) {
	_, ln := listen(t)
	ctx, cancel := context.WithTimeout(context.Background(), 200*time.Millisecond)
	defer cancel()
	start := time.Now()
	req, err := ln.Accept(ctx)
	if req != nil || !errors.Is(err, context.DeadlineExceeded) {
		t.Fatalf("idle Accept() = %v, %v; want nil, context.DeadlineExceeded", req, err)
	}
	if elapsed := time.Since(start); elapsed > 2*time.Second {
		t.Errorf("idle Accept() returned after %v, want about 200ms", elapsed)
	}
	start = time.Now()
	if err := ln.Close(); err != nil {
		t.Fatalf("Close() error: %v", err)
	}
	if elapsed := time.Since(start); elapsed > time.Second {
		t.Errorf("Close() took %v after the cancelled Accept, want immediate", elapsed)
	}
	if _, err := ln.Accept(context.Background()); !errors.Is(err, net.ErrClosed) {
		t.Errorf("Accept() after Close() error = %v, want net.ErrClosed", err)
	}
}

func TestConnectCancelThenClose(t *testing.T) {
	// The listener never accepts, so the request stays pending.
	dev, ln := listen(t)
	client := resolve(t, dev, ln.Addr())
	ctx, cancel := context.WithTimeout(context.Background(), 300*time.Millisecond)
	defer cancel()
	start := time.Now()
	info, err := client.Connect(ctx, nil)
	if info != nil || !errors.Is(err, context.DeadlineExceeded) {
		t.Fatalf("pending Connect() = %v, %v; want nil, context.DeadlineExceeded", info, err)
	}
	if elapsed := time.Since(start); elapsed > 2*time.Second {
		t.Errorf("pending Connect() returned after %v, want about 300ms", elapsed)
	}
	start = time.Now()
	if err := client.Close(); err != nil {
		t.Fatalf("Close() error: %v", err)
	}
	if elapsed := time.Since(start); elapsed > time.Second {
		t.Errorf("Close() took %v after the cancelled Connect, want immediate", elapsed)
	}
}

func TestListenerCloseUnblocksAccept(t *testing.T) {
	_, ln := listen(t)
	errc := make(chan error, 1)
	go func() {
		_, err := ln.Accept(context.Background())
		errc <- err
	}()
	time.Sleep(100 * time.Millisecond)
	if err := ln.Close(); err != nil {
		t.Fatalf("Close() error: %v", err)
	}
	select {
	case err := <-errc:
		if !errors.Is(err, net.ErrClosed) {
			t.Errorf("Accept() error after Close() = %v, want net.ErrClosed", err)
		}
	case <-time.After(5 * time.Second):
		t.Fatal("Accept() did not return after Close()")
	}
}

func threadCount(t *testing.T) int {
	t.Helper()
	status, err := os.ReadFile("/proc/self/status")
	if err != nil {
		t.Fatalf("read /proc/self/status: %v", err)
	}
	for _, line := range strings.Split(string(status), "\n") {
		if n, ok := strings.CutPrefix(line, "Threads:"); ok {
			c, err := strconv.Atoi(strings.TrimSpace(n))
			if err != nil {
				t.Fatalf("parse %q: %v", line, err)
			}
			return c
		}
	}
	t.Fatal("no Threads line in /proc/self/status")
	return 0
}

func TestAcceptDoesNotHoldThreads(t *testing.T) {
	dev := rdmatest.Device(t)
	const waiters = 100
	lns := make([]*cm.Listener, waiters)
	for i := range lns {
		ln, err := cm.Listen(netip.AddrPortFrom(dev.IP, 0), 1)
		if err != nil {
			t.Fatalf("Listen() #%d error: %v", i, err)
		}
		defer ln.Close()
		lns[i] = ln
	}

	before := threadCount(t)
	ctx, cancel := context.WithCancel(context.Background())
	var wg sync.WaitGroup
	errs := make(chan error, waiters)
	for _, ln := range lns {
		wg.Add(1)
		go func() {
			defer wg.Done()
			_, err := ln.Accept(ctx)
			errs <- err
		}()
	}
	time.Sleep(500 * time.Millisecond)
	after := threadCount(t)
	cancel()
	wg.Wait()
	close(errs)
	for err := range errs {
		if !errors.Is(err, context.Canceled) {
			t.Errorf("Accept() error = %v, want context.Canceled", err)
		}
	}
	if after-before >= 10 {
		t.Errorf("Threads grew from %d to %d with %d idle Accepts, want growth < 10", before, after, waiters)
	}
}

func TestGoroutinesReturnToBaseline(t *testing.T) {
	rdmatest.Device(t)
	before := runtime.NumGoroutine()
	t.Run("connections", func(t *testing.T) {
		ln, client, server := pair(t)
		ctx, cancel := context.WithTimeout(context.Background(), timeout)
		defer cancel()
		errc := make(chan error, 1)
		go func() { errc <- client.Disconnect(ctx) }()
		if ev, err := server.ID.Wait(ctx); err != nil || ev != cm.EventDisconnected {
			t.Fatalf("Wait() = %v, %v; want %v", ev, err, cm.EventDisconnected)
		}
		if err := server.ID.Disconnect(ctx); err != nil {
			t.Errorf("server Disconnect() error: %v", err)
		}
		if err := <-errc; err != nil {
			t.Errorf("client Disconnect() error: %v", err)
		}
		idle, cancelIdle := context.WithTimeout(ctx, 100*time.Millisecond)
		defer cancelIdle()
		if _, err := ln.Accept(idle); !errors.Is(err, context.DeadlineExceeded) {
			t.Errorf("idle Accept() error = %v, want context.DeadlineExceeded", err)
		}
	})
	deadline := time.Now().Add(5 * time.Second)
	for time.Now().Before(deadline) {
		if runtime.NumGoroutine() <= before {
			return
		}
		time.Sleep(10 * time.Millisecond)
	}
	t.Errorf("goroutines: %d before, %d after closing every object", before, runtime.NumGoroutine())
}
