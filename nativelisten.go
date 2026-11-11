package rdma

import (
	"context"
	"net"
	"net/netip"
	"sync"

	"github.com/wzshiming/rdma/cm"
)

// requestListener is what a native acceptor takes the connection requests
// from: a listener of cm, or a stub in tests.
type requestListener interface {
	Accept(ctx context.Context) (*cm.Request, error)
	Addr() netip.AddrPort
	Close() error
}

// nativeAcceptor is the acceptor of the native engine. It takes the requests
// off its listener as they arrive and has handshake establish each on its
// own, so that a handshake that hangs delays no other; accept hands out the
// streams established.
type nativeAcceptor struct {
	l         requestListener
	handshake func(ctx context.Context, req *cm.Request) (stream, error)
	ctx       context.Context // ends with close
	cancel    context.CancelFunc
	ready     chan incoming
	running   sync.WaitGroup // run and the handshakes under way
}

// incoming is what an accept returns: a stream established, or an error of
// the listener.
type incoming struct {
	s   stream
	err error
}

func newNativeAcceptor(l requestListener, handshake func(ctx context.Context, req *cm.Request) (stream, error)) *nativeAcceptor {
	a := &nativeAcceptor{l: l, handshake: handshake, ready: make(chan incoming)}
	a.ctx, a.cancel = context.WithCancel(context.Background())
	a.running.Add(1)
	go a.run()
	return a
}

// run takes the requests off the listener and starts the handshake of each;
// an error of the listener goes to an accept.
func (a *nativeAcceptor) run() {
	defer a.running.Done()
	for {
		req, err := a.l.Accept(a.ctx)
		if err != nil {
			if a.ctx.Err() != nil || !a.hand(incoming{err: err}) {
				return
			}
			continue
		}
		a.running.Add(1)
		go a.establish(req)
	}
}

// establish hands accept the stream of req once handshake has established
// it; a request that fails, rejected among them, never reaches accept. A
// stream nobody takes before close goes down with the acceptor.
func (a *nativeAcceptor) establish(req *cm.Request) {
	defer a.running.Done()
	s, err := a.handshake(a.ctx, req)
	if err != nil {
		return
	}
	if !a.hand(incoming{s: s}) {
		s.close()
	}
}

// hand waits for an accept to take in, and reports false when close comes
// first.
func (a *nativeAcceptor) hand(in incoming) bool {
	select {
	case a.ready <- in:
		return true
	case <-a.ctx.Done():
		return false
	}
}

func (a *nativeAcceptor) addr() netip.AddrPort { return a.l.Addr() }

func (a *nativeAcceptor) accept() (stream, error) {
	if a.ctx.Err() != nil {
		return nil, net.ErrClosed
	}
	select {
	case in := <-a.ready:
		return in.s, in.err
	case <-a.ctx.Done():
		return nil, net.ErrClosed
	}
}

// close stops listening, abandons the handshakes under way and returns once
// they have ended.
func (a *nativeAcceptor) close() error {
	a.cancel()
	err := a.l.Close()
	a.running.Wait()
	return err
}
