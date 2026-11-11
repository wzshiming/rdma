package rdma

import (
	"context"
	"errors"
	"net"
	"net/netip"
	"sync"
	"testing"
	"time"

	"github.com/wzshiming/rdma/cm"
)

// stubListener hands an acceptor the requests and the errors a test pushes,
// as a listener of cm hands out the requests that arrive.
type stubListener struct {
	next   chan *cm.Request
	errs   chan error
	closed chan struct{}
	once   sync.Once
}

func newStubListener() *stubListener {
	return &stubListener{next: make(chan *cm.Request, 16), errs: make(chan error, 16), closed: make(chan struct{})}
}

func (l *stubListener) push(req *cm.Request) { l.next <- req }

func (l *stubListener) fail(err error) { l.errs <- err }

func (l *stubListener) Accept(ctx context.Context) (*cm.Request, error) {
	select {
	case req := <-l.next:
		return req, nil
	case err := <-l.errs:
		return nil, err
	case <-ctx.Done():
		return nil, ctx.Err()
	case <-l.closed:
		return nil, net.ErrClosed
	}
}

func (l *stubListener) Addr() netip.AddrPort { return netip.MustParseAddrPort("10.99.0.1:7471") }

func (l *stubListener) Close() error {
	l.once.Do(func() { close(l.closed) })
	return nil
}

// stubStream is a stream an acceptor hands out, of whose calls it makes
// only close.
type stubStream struct {
	stream
	closed chan struct{}
	once   sync.Once
}

func newStubStream() *stubStream { return &stubStream{closed: make(chan struct{})} }

func (s *stubStream) close() error {
	s.once.Do(func() { close(s.closed) })
	return nil
}

// acceptedStream is what an accept of an acceptor returned.
type acceptedStream struct {
	s   stream
	err error
}

// acceptFrom runs a.accept in the background.
func acceptFrom(a acceptor) <-chan acceptedStream {
	ch := make(chan acceptedStream, 1)
	go func() {
		s, err := a.accept()
		ch <- acceptedStream{s, err}
	}()
	return ch
}

// awaitAccepted waits at most 5 seconds for the accept of ch.
func awaitAccepted(t *testing.T, ch <-chan acceptedStream) acceptedStream {
	t.Helper()
	select {
	case r := <-ch:
		return r
	case <-time.After(5 * time.Second):
		t.Fatal("accept did not return within 5 seconds")
		return acceptedStream{}
	}
}

// A request whose handshake hangs does not delay the acceptance of the
// requests after it: each is established on its own.
func TestNativeAcceptorDoesNotWaitForAHangingHandshake(t *testing.T) {
	l := newStubListener()
	hanging, later := &cm.Request{}, &cm.Request{}
	want := newStubStream()
	a := newNativeAcceptor(l, func(ctx context.Context, req *cm.Request) (stream, error) {
		if req == hanging {
			<-ctx.Done()
			return nil, ctx.Err()
		}
		return want, nil
	})
	defer a.close()
	l.push(hanging)
	l.push(later)
	if r := awaitAccepted(t, acceptFrom(a)); r.s != want || r.err != nil {
		t.Errorf("accept with the first request's handshake hanging = %v, %v; want the later request's stream", r.s, r.err)
	}
}

// A request whose handshake fails, as a rejected one does, never reaches
// accept, which hands out the next request's stream.
func TestNativeAcceptorLeavesOutFailedHandshakes(t *testing.T) {
	l := newStubListener()
	rejected, next := &cm.Request{}, &cm.Request{}
	want := newStubStream()
	failed := make(chan struct{})
	a := newNativeAcceptor(l, func(ctx context.Context, req *cm.Request) (stream, error) {
		if req == rejected {
			defer close(failed)
			return nil, errors.New("rejected")
		}
		return want, nil
	})
	defer a.close()
	l.push(rejected)
	<-failed
	l.push(next)
	if r := awaitAccepted(t, acceptFrom(a)); r.s != want || r.err != nil {
		t.Errorf("accept after a failed handshake = %v, %v; want the next request's stream", r.s, r.err)
	}
}

// Close wakes an accept waiting for a stream with net.ErrClosed, which an
// accept after it returns at once.
func TestNativeAcceptorCloseUnblocksAccept(t *testing.T) {
	a := newNativeAcceptor(newStubListener(), nil)
	waiting := acceptFrom(a)
	if err := a.close(); err != nil {
		t.Fatalf("close() error: %v", err)
	}
	for _, ch := range []<-chan acceptedStream{waiting, acceptFrom(a)} {
		if r := awaitAccepted(t, ch); r.s != nil || !errors.Is(r.err, net.ErrClosed) {
			t.Errorf("accept with the acceptor closed = %v, %v; want net.ErrClosed", r.s, r.err)
		}
	}
}

// Close takes down the connections established that nobody accepted.
func TestNativeAcceptorCloseTakesDownTheUnaccepted(t *testing.T) {
	l := newStubListener()
	s := newStubStream()
	established := make(chan struct{})
	a := newNativeAcceptor(l, func(ctx context.Context, req *cm.Request) (stream, error) {
		defer close(established)
		return s, nil
	})
	l.push(&cm.Request{})
	<-established
	if err := a.close(); err != nil {
		t.Fatalf("close() error: %v", err)
	}
	select {
	case <-s.closed:
	case <-time.After(5 * time.Second):
		t.Error("the stream nobody accepted is still open 5 seconds after close")
	}
}

// Close abandons the handshakes under way and returns once they have ended,
// so that nothing they set up outlives it.
func TestNativeAcceptorCloseWaitsForTheHandshakes(t *testing.T) {
	l := newStubListener()
	started, ended := make(chan struct{}), make(chan struct{})
	a := newNativeAcceptor(l, func(ctx context.Context, req *cm.Request) (stream, error) {
		close(started)
		<-ctx.Done()
		time.Sleep(50 * time.Millisecond) // tearing down what it set up
		close(ended)
		return nil, ctx.Err()
	})
	l.push(&cm.Request{})
	<-started
	if err := a.close(); err != nil {
		t.Fatalf("close() error: %v", err)
	}
	select {
	case <-ended:
	default:
		t.Error("close returned before the handshake under way ended")
	}
}

// An error of the listener, such as a failure to take a request off it,
// reaches accept, and the requests after it are accepted still.
func TestNativeAcceptorReportsListenerErrors(t *testing.T) {
	l := newStubListener()
	want := newStubStream()
	a := newNativeAcceptor(l, func(ctx context.Context, req *cm.Request) (stream, error) { return want, nil })
	defer a.close()
	failure := errors.New("rdma_create_event_channel: too many open files")
	l.fail(failure)
	if r := awaitAccepted(t, acceptFrom(a)); r.s != nil || !errors.Is(r.err, failure) {
		t.Errorf("accept with the listener failing = %v, %v; want the listener's error %q", r.s, r.err, failure)
	}
	l.push(&cm.Request{})
	if r := awaitAccepted(t, acceptFrom(a)); r.s != want || r.err != nil {
		t.Errorf("accept after the listener failed = %v, %v; want the next request's stream", r.s, r.err)
	}
}

// The acceptor's address is the one its listener bound.
func TestNativeAcceptorAddrIsTheListeners(t *testing.T) {
	l := newStubListener()
	a := newNativeAcceptor(l, nil)
	defer a.close()
	if got := a.addr(); got != l.Addr() {
		t.Errorf("addr() = %v, want the listener's %v", got, l.Addr())
	}
}
