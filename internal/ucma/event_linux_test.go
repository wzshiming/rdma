//go:build linux && !android && (amd64 || arm64 || loong64 || ppc64le || riscv64 || s390x)

package ucma

import (
	"bytes"
	"context"
	"encoding/binary"
	"os"
	"reflect"
	"runtime"
	"strconv"
	"strings"
	"sync"
	"syscall"
	"testing"
	"time"
	"unsafe"

	"github.com/wzshiming/rdma/internal/rdmaabi"
)

// testConnEvent is an event in the connection form.
func testConnEvent() rdmaabi.UcmaAbiEventResp {
	ev := rdmaabi.UcmaAbiEventResp{Uid: 0x1122334455667788, Id: 9, Event: uint32(EventConnectRequest)}
	ev.Param.Conn = rdmaabi.UcmaAbiConnParam{Qp_num: 0x123456, Private_data: privateData("hello"), Private_data_len: 5, Srq: 1, Responder_resources: 2, Initiator_depth: 3, Flow_control: 1, Retry_count: 6, Rnr_retry_count: 5}
	ev.Ece = rdmaabi.UcmaAbiEce{Vendor_id: 0x02c9, Attr_mod: 0x1234}
	return ev
}

// testUDEvent is an event in the datagram form.
func testUDEvent() rdmaabi.UcmaAbiEventResp {
	ev := rdmaabi.UcmaAbiEventResp{Uid: 5, Id: 10, Event: uint32(EventEstablished)}
	*(*rdmaabi.UcmaAbiUdParam)(unsafe.Pointer(&ev.Param)) = rdmaabi.UcmaAbiUdParam{
		Qp_num: 0x654321,
		Qkey:   0x11111111,
		Ah_attr: rdmaabi.IbUverbsAhAttr{
			Grh:       rdmaabi.IbUverbsGlobalRoute{Dgid: [16]byte{0xff, 0x0e, 15: 7}, Flow_label: 0x54321, Sgid_index: 2, Hop_limit: 1, Traffic_class: 4},
			Dlid:      0xc000,
			Sl:        3,
			Is_global: 1,
			Port_num:  1,
		},
		Private_data:     privateData("datagram"),
		Private_data_len: 8,
	}
	return ev
}

func timeoutContext(t *testing.T) context.Context {
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	t.Cleanup(cancel)
	return ctx
}

// GET_EVENT is one write of the whole request with a response the size of
// struct ucma_abi_event_resp; the kernel wants 324 bytes at least.
func TestGetEventEncoding(t *testing.T) {
	d := newFakeDevice(t)
	ev := testConnEvent()
	d.queue(t, &ev)
	if _, err := d.ch.GetEvent(timeoutContext(t)); err != nil {
		t.Fatalf("GetEvent() error: %v", err)
	}
	cmds := d.commands()
	if len(cmds) != 1 {
		t.Fatalf("%d writes, want 1", len(cmds))
	}
	got := cmds[0]
	if outOf(got) < 324 || binary.NativeEndian.Uint64(got[8:]) == 0 {
		t.Errorf("response of %d bytes at %#x, want 324 bytes at least at an address", outOf(got), binary.NativeEndian.Uint64(got[8:]))
	}
	clear(got[8:16])
	if want := bytesOf(&rdmaabi.UcmaAbiGetEvent{Cmd: 12, In: 8, Out: 336}); !bytes.Equal(got, want) {
		t.Errorf("wrote % x, want % x", got, want)
	}
}

func TestGetEventDecodesTheConnectionForm(t *testing.T) {
	d := newFakeDevice(t)
	resp := testConnEvent()
	status := -int32(syscall.ETIMEDOUT)
	resp.Status = uint32(status)
	d.queue(t, &resp)
	ev, err := d.ch.GetEvent(timeoutContext(t))
	if err != nil {
		t.Fatalf("GetEvent() error: %v", err)
	}
	if ev.Type != EventConnectRequest || ev.Status != -int32(syscall.ETIMEDOUT) || ev.UID != 0x1122334455667788 || ev.ID != 9 || ev.ECE != testECE {
		t.Errorf("GetEvent() = %+v; want CONNECT_REQUEST, status -ETIMEDOUT, uid 0x1122334455667788, id 9, ECE %+v", ev, testECE)
	}
	if got := ev.Conn(); !reflect.DeepEqual(got, testConn) {
		t.Errorf("Conn() = %+v, want %+v", got, testConn)
	}
}

func TestGetEventDecodesTheDatagramForm(t *testing.T) {
	d := newFakeDevice(t)
	resp := testUDEvent()
	d.queue(t, &resp)
	ev, err := d.ch.GetEvent(timeoutContext(t))
	if err != nil {
		t.Fatalf("GetEvent() error: %v", err)
	}
	want := UDParam{
		PrivateData: []byte("datagram"),
		QPNum:       0x654321,
		QKey:        0x11111111,
		AHAttr: rdmaabi.IbUverbsAhAttr{
			Grh:       rdmaabi.IbUverbsGlobalRoute{Dgid: [16]byte{0xff, 0x0e, 15: 7}, Flow_label: 0x54321, Sgid_index: 2, Hop_limit: 1, Traffic_class: 4},
			Dlid:      0xc000,
			Sl:        3,
			Is_global: 1,
			Port_num:  1,
		},
	}
	if got := ev.UD(); ev.Type != EventEstablished || ev.UID != 5 || ev.ID != 10 || !reflect.DeepEqual(got, want) {
		t.Errorf("GetEvent() = %+v with UD() %+v; want ESTABLISHED, uid 5, id 10, %+v", ev, got, want)
	}
}

func TestEventWithoutPrivateData(t *testing.T) {
	d := newFakeDevice(t)
	d.queue(t, &rdmaabi.UcmaAbiEventResp{Event: uint32(EventDisconnected)})
	ev, err := d.ch.GetEvent(timeoutContext(t))
	if err != nil || ev.Conn().PrivateData != nil || ev.UD().PrivateData != nil {
		t.Errorf("GetEvent() = %+v, %v; want no private data in either form", ev, err)
	}
}

func TestEventTypeString(t *testing.T) {
	for typ, want := range map[EventType]string{
		EventAddrResolved:   "ADDR_RESOLVED",
		EventConnectRequest: "CONNECT_REQUEST",
		EventRejected:       "REJECTED",
		EventTimewaitExit:   "TIMEWAIT_EXIT",
		16:                  "EventType(16)",
	} {
		if got := typ.String(); got != want {
			t.Errorf("EventType(%d).String() = %q, want %q", typ, got, want)
		}
	}
}

func TestGetEventWriteErrors(t *testing.T) {
	d := newFakeDevice(t)
	d.setReply(func(fd uintptr, cmd []byte) (int, syscall.Errno) { return len(cmd) - 1, 0 })
	if _, err := d.ch.GetEvent(timeoutContext(t)); err != syscall.ENODATA {
		t.Errorf("short write: error %#v, want syscall.ENODATA", err)
	}
	d.setReply(func(fd uintptr, cmd []byte) (int, syscall.Errno) { return -1, syscall.EFAULT })
	if _, err := d.ch.GetEvent(timeoutContext(t)); err != syscall.EFAULT {
		t.Errorf("failed write: error %#v, want syscall.EFAULT", err)
	}
}

// The event, written by the kernel during the write, lands where the
// request points although the stack moves and garbage is collected.
func TestEventMemoryIsPinnedForTheWrite(t *testing.T) {
	d := newFakeDevice(t)
	resp := testConnEvent()
	d.setReply(func(fd uintptr, cmd []byte) (int, syscall.Errno) {
		growStack(256)
		runtime.GC()
		respond(cmd, bytesOf(&resp))
		return len(cmd), 0
	})
	ev, err := d.ch.GetEvent(timeoutContext(t))
	if err != nil || ev.UID != resp.Uid || ev.ID != resp.Id || ev.Type != EventConnectRequest {
		t.Errorf("GetEvent() = %+v, %v; want uid %#x, id %d, CONNECT_REQUEST", ev, err, resp.Uid, resp.Id)
	}
}

func TestGetEventWaitsForAnEvent(t *testing.T) {
	d := newFakeDevice(t)
	resp := testConnEvent()
	go func() {
		time.Sleep(50 * time.Millisecond)
		d.queue(t, &resp)
	}()
	if ev, err := d.ch.GetEvent(timeoutContext(t)); err != nil || ev.ID != 9 {
		t.Fatalf("GetEvent() = %+v, %v; want the event of id 9", ev, err)
	}
}

func TestGetEventReturnsTheContextError(t *testing.T) {
	d := newFakeDevice(t)
	ctx, cancel := context.WithTimeout(context.Background(), 100*time.Millisecond)
	defer cancel()
	start := time.Now()
	if _, err := d.ch.GetEvent(ctx); err != context.DeadlineExceeded {
		t.Fatalf("GetEvent() error = %v, want context.DeadlineExceeded", err)
	}
	if elapsed := time.Since(start); elapsed > 2*time.Second {
		t.Errorf("GetEvent() took %v, want about 100ms", elapsed)
	}
	// The ended wait leaves nothing behind that cuts the next one short.
	resp := testConnEvent()
	go func() {
		time.Sleep(50 * time.Millisecond)
		d.queue(t, &resp)
	}()
	if _, err := d.ch.GetEvent(timeoutContext(t)); err != nil {
		t.Fatalf("second GetEvent() error: %v", err)
	}
	done, cancelDone := context.WithCancel(context.Background())
	cancelDone()
	if _, err := d.ch.GetEvent(done); err != context.Canceled {
		t.Errorf("GetEvent(cancelled) error = %v, want context.Canceled", err)
	}
}

func TestCloseWakesGetEvent(t *testing.T) {
	d := newFakeDevice(t)
	errc := make(chan error, 1)
	go func() {
		_, err := d.ch.GetEvent(context.Background())
		errc <- err
	}()
	time.Sleep(50 * time.Millisecond)
	if err := d.ch.Close(); err != nil {
		t.Fatal(err)
	}
	select {
	case err := <-errc:
		if err != os.ErrClosed {
			t.Errorf("GetEvent() error = %v, want os.ErrClosed", err)
		}
	case <-time.After(5 * time.Second):
		t.Fatal("GetEvent() did not return after Close()")
	}
	if _, err := d.ch.GetEvent(context.Background()); err != os.ErrClosed {
		t.Errorf("GetEvent() after Close() error = %v, want os.ErrClosed", err)
	}
}

// A second waiter queues behind the first and still honours its context.
func TestGetEventQueuedWaiterHonoursItsContext(t *testing.T) {
	d := newFakeDevice(t)
	first := make(chan error, 1)
	go func() {
		_, err := d.ch.GetEvent(context.Background())
		first <- err
	}()
	time.Sleep(50 * time.Millisecond)
	ctx, cancel := context.WithTimeout(context.Background(), 100*time.Millisecond)
	defer cancel()
	if _, err := d.ch.GetEvent(ctx); err != context.DeadlineExceeded {
		t.Errorf("queued GetEvent() error = %v, want context.DeadlineExceeded", err)
	}
	resp := testConnEvent()
	d.queue(t, &resp)
	select {
	case err := <-first:
		if err != nil {
			t.Errorf("first GetEvent() error: %v", err)
		}
	case <-time.After(5 * time.Second):
		t.Fatal("first GetEvent() did not return after the event")
	}
}

// Waiters take turns: two waiters and two events, one event each.
func TestGetEventWaitersTakeTurns(t *testing.T) {
	d := newFakeDevice(t)
	ctx := timeoutContext(t)
	ids := make(chan uint32, 2)
	for range 2 {
		go func() {
			ev, err := d.ch.GetEvent(ctx)
			if err != nil {
				t.Error(err)
			}
			ids <- ev.ID
		}()
	}
	for id := range uint32(2) {
		resp := testConnEvent()
		resp.Id = id + 1
		d.queue(t, &resp)
	}
	if a, b := <-ids, <-ids; a+b != 3 || a == b {
		t.Errorf("the waiters got the events of ids %d and %d, want 1 and 2", a, b)
	}
}

func threadCount(t *testing.T) int {
	t.Helper()
	status, err := os.ReadFile("/proc/self/status")
	if err != nil {
		t.Fatal(err)
	}
	for line := range strings.Lines(string(status)) {
		if n, ok := strings.CutPrefix(line, "Threads:"); ok {
			c, err := strconv.Atoi(strings.TrimSpace(n))
			if err != nil {
				t.Fatal(err)
			}
			return c
		}
	}
	t.Fatal("no Threads line in /proc/self/status")
	return 0
}

// Waiting is the runtime poller's: a hundred waiters on idle channels hold
// no OS thread.
func TestWaitersHoldNoThreads(t *testing.T) {
	const waiters = 100
	devs := make([]*fakeDevice, waiters)
	for i := range devs {
		devs[i] = newFakeDevice(t)
	}
	before := threadCount(t)
	ctx, cancel := context.WithCancel(context.Background())
	var wg sync.WaitGroup
	errs := make(chan error, waiters)
	for _, d := range devs {
		wg.Go(func() {
			_, err := d.ch.GetEvent(ctx)
			errs <- err
		})
	}
	time.Sleep(500 * time.Millisecond)
	after := threadCount(t)
	cancel()
	wg.Wait()
	close(errs)
	for err := range errs {
		if err != context.Canceled {
			t.Errorf("GetEvent() error = %v, want context.Canceled", err)
		}
	}
	if after-before >= 10 {
		t.Errorf("threads grew from %d to %d with %d idle waiters, want growth < 10", before, after, waiters)
	}
}
