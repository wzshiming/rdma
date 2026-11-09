//go:build linux && cgo

package rdma

import (
	"context"
	"errors"
	"fmt"
	"net/netip"
	"sync"
	"sync/atomic"
	"syscall"

	"github.com/wzshiming/rdma/cm"
	"github.com/wzshiming/rdma/verbs"
)

// registeredBytes counts the registered buffer memory of live connections.
var registeredBytes atomic.Int64

// platformFabric returns the fabric built on cm and verbs.
func platformFabric() fabric { return rcFabric{} }

// rcFabric connects RC queue pairs through rdma_cm.
type rcFabric struct{}

func (rcFabric) dial(ctx context.Context, local, remote netip.AddrPort, cfg Config, hello []byte) (transport, []byte, error) {
	id, err := cm.Resolve(ctx, local, remote)
	if err != nil {
		return nil, nil, err
	}
	t, err := newRCTransport(id, cfg)
	if err != nil {
		id.Close()
		t.release()
		return nil, nil, err
	}
	info, err := id.Connect(ctx, &cm.ConnParam{PrivateData: hello})
	if err != nil {
		t.close()
		return nil, nil, err
	}
	t.start()
	return t, info.PrivateData, nil
}

func (rcFabric) listen(local netip.AddrPort, backlog int) (acceptor, error) {
	ln, err := cm.Listen(local, backlog)
	if err != nil {
		return nil, err
	}
	return &rcAcceptor{ln}, nil
}

type rcAcceptor struct{ ln *cm.Listener }

func (a *rcAcceptor) addr() netip.AddrPort { return a.ln.Addr() }

func (a *rcAcceptor) accept(ctx context.Context) (request, error) {
	req, err := a.ln.Accept(ctx)
	if err != nil {
		return nil, err
	}
	return &rcRequest{req}, nil
}

func (a *rcAcceptor) close() error { return a.ln.Close() }

type rcRequest struct{ req *cm.Request }

func (r *rcRequest) hello() []byte              { return r.req.PrivateData }
func (r *rcRequest) remoteAddr() netip.AddrPort { return r.req.ID.RemoteAddr() }

func (r *rcRequest) accept(ctx context.Context, cfg Config, reply []byte) (transport, error) {
	t, err := newRCTransport(r.req.ID, cfg)
	if err != nil {
		// The dialer learns at once instead of timing out.
		r.req.Reject(encodeReject(rejectResources))
		t.release()
		return nil, err
	}
	if err := r.req.Accept(ctx, &cm.ConnParam{PrivateData: reply}); err != nil {
		t.close()
		return nil, err
	}
	t.start()
	return t, nil
}

func (r *rcRequest) reject(data []byte) error { return r.req.Reject(data) }

// sendIDFlag marks the work request IDs of sends; the rest of the ID is the
// slot, so completions name their buffer even when flushed.
const sendIDFlag = 1 << 32

// rcTransport is an RC queue pair with registered send and receive buffers.
// A goroutine watches the connection manager for the peer's disconnect; the
// completion channel is waited on by the engine through wait.
type rcTransport struct {
	id             *cm.ID
	pd             *verbs.PD
	ch             *verbs.CompChannel
	cq             *verbs.CQ
	qp             *verbs.QP
	sendMR, recvMR *verbs.MR
	sendBuf        []byte
	recvBuf        []byte
	msgSize        int
	inline         int        // largest message the device sends inline
	wcs            []verbs.WC // scratch for poll
	registered     int64

	failCtx    context.Context // ends when the transport fails
	failCancel context.CancelFunc
	failOnce   sync.Once
	failErr    error

	watchCtx     context.Context
	watchCancel  context.CancelFunc
	watchDone    chan struct{}
	disconnected atomic.Bool
}

// newRCTransport allocates the resources of a connection on id and posts all
// receive buffers. On error the returned transport holds what was allocated,
// for release after the caller has closed id, which owns the queue pair.
func newRCTransport(id *cm.ID, cfg Config) (*rcTransport, error) {
	t := &rcTransport{id: id, msgSize: cfg.MessageSize, wcs: make([]verbs.WC, 64)}
	t.failCtx, t.failCancel = context.WithCancel(context.Background())
	dev := id.Device()
	nsend, nrecv := cfg.SendDepth+1, cfg.ReceiveDepth+1
	if err := checkLimits(dev, id.Port(), cfg, nsend, nrecv); err != nil {
		return t, err
	}
	var err error
	if t.pd, err = dev.AllocPD(); err != nil {
		return t, err
	}
	if t.ch, err = dev.CreateCompChannel(); err != nil {
		return t, err
	}
	if t.cq, err = dev.CreateCQ(nsend+nrecv, t.ch); err != nil {
		return t, err
	}
	attr := verbs.QPInitAttr{
		SendCQ:    t.cq,
		RecvCQ:    t.cq,
		Type:      verbs.QPTypeRC,
		Cap:       verbs.QPCap{MaxSendWR: uint32(nsend), MaxRecvWR: uint32(nrecv), MaxSendSGE: 1, MaxRecvSGE: 1, MaxInlineData: uint32(cfg.MaxInline)},
		SignalAll: true,
	}
	if t.qp, err = id.CreateQP(t.pd, &attr); err != nil {
		return t, err
	}
	t.inline = int(attr.Cap.MaxInlineData)
	if t.sendMR, err = t.pd.AllocMemory(nsend*cfg.MessageSize, verbs.AccessLocalWrite); err != nil {
		return t, err
	}
	t.registered += int64(nsend * cfg.MessageSize)
	if t.recvMR, err = t.pd.AllocMemory(nrecv*cfg.MessageSize, verbs.AccessLocalWrite); err != nil {
		return t, err
	}
	t.registered += int64(nrecv * cfg.MessageSize)
	registeredBytes.Add(t.registered)
	t.sendBuf, t.recvBuf = t.sendMR.Bytes(), t.recvMR.Bytes()
	wrs := make([]verbs.RecvWR, nrecv)
	for i := range wrs {
		wrs[i] = verbs.RecvWR{ID: uint64(i), SGE: []verbs.SGE{t.recvMR.SGE(t.recvSlot(i))}}
	}
	if err := t.qp.PostRecv(wrs...); err != nil {
		return t, err
	}
	return t, nil
}

// checkLimits rejects a configuration the device cannot serve.
func checkLimits(dev *verbs.Context, port uint8, cfg Config, nsend, nrecv int) error {
	attr, err := dev.QueryDevice()
	if err != nil {
		return err
	}
	pattr, err := dev.QueryPort(port)
	if err != nil {
		return err
	}
	switch {
	case uint64(cfg.MessageSize) > uint64(pattr.MaxMsgSize):
		return fmt.Errorf("rdma: MessageSize %d exceeds the port's maximum message size %d", cfg.MessageSize, pattr.MaxMsgSize)
	case uint64(nsend*cfg.MessageSize) > attr.MaxMRSize || uint64(nrecv*cfg.MessageSize) > attr.MaxMRSize:
		return fmt.Errorf("rdma: %d messages of %d bytes exceed the device's maximum region size %d", max(nsend, nrecv), cfg.MessageSize, attr.MaxMRSize)
	case nsend > attr.MaxQPWR || nrecv > attr.MaxQPWR:
		return fmt.Errorf("rdma: SendDepth %d or ReceiveDepth %d exceeds the device's queue limit of %d work requests", cfg.SendDepth, cfg.ReceiveDepth, attr.MaxQPWR-1)
	case nsend+nrecv > attr.MaxCQE:
		return fmt.Errorf("rdma: SendDepth %d plus ReceiveDepth %d exceed the device's completion queue limit of %d", cfg.SendDepth, cfg.ReceiveDepth, attr.MaxCQE-2)
	}
	return nil
}

// start watches the connection for the peer's disconnect.
func (t *rcTransport) start() {
	t.watchCtx, t.watchCancel = context.WithCancel(context.Background())
	t.watchDone = make(chan struct{})
	go t.watch()
}

func (t *rcTransport) watch() {
	defer close(t.watchDone)
	for {
		ev, err := t.id.Wait(t.watchCtx)
		if err != nil {
			return
		}
		switch ev {
		case cm.EventDisconnected:
			// Answer so that the peer's disconnect completes, and move the
			// queue pair to the error state, which flushes our queues.
			t.doDisconnect(context.Background())
			t.fail(errConnReset)
			return
		case cm.EventDeviceRemoval:
			t.fail(syscall.ENODEV)
			return
		}
	}
}

// stopWatch ends the watcher so that the ID's event channel is free.
func (t *rcTransport) stopWatch() {
	if t.watchCancel == nil {
		return
	}
	t.watchCancel()
	<-t.watchDone
	t.watchCancel = nil
}

// fail records the first failure and wakes a pending wait.
func (t *rcTransport) fail(err error) {
	t.failOnce.Do(func() {
		t.failErr = err
		t.failCancel()
	})
}

func (t *rcTransport) sendSlot(i int) []byte {
	return t.sendBuf[i*t.msgSize : (i+1)*t.msgSize : (i+1)*t.msgSize]
}

func (t *rcTransport) recvSlot(i int) []byte {
	return t.recvBuf[i*t.msgSize : (i+1)*t.msgSize : (i+1)*t.msgSize]
}

func (t *rcTransport) postSend(i, n int) error {
	flags := verbs.SendSignaled
	if n <= t.inline {
		flags |= verbs.SendInline
	}
	return t.qp.PostSend(verbs.SendWR{
		ID:    uint64(i) | sendIDFlag,
		Op:    verbs.WRSend,
		SGE:   []verbs.SGE{t.sendMR.SGE(t.sendSlot(i)[:n])},
		Flags: flags,
	})
}

func (t *rcTransport) postRecv(i int) error {
	return t.qp.PostRecv(verbs.RecvWR{ID: uint64(i), SGE: []verbs.SGE{t.recvMR.SGE(t.recvSlot(i))}})
}

// wait polls the completion queue, arms the channel, polls again to close
// the race and then sleeps on the channel until ctx ends, the transport
// fails or a completion event arrives.
func (t *rcTransport) wait(ctx context.Context, wcs []completion) (int, error) {
	for {
		if n, err := t.poll(wcs); n > 0 || err != nil {
			return n, err
		}
		if t.failCtx.Err() != nil {
			return 0, t.failErr
		}
		if err := t.cq.RequestNotify(false); err != nil {
			return 0, err
		}
		if n, err := t.poll(wcs); n > 0 || err != nil {
			return n, err
		}
		wctx, cancel := context.WithCancel(t.failCtx)
		stop := context.AfterFunc(ctx, cancel)
		_, err := t.ch.Wait(wctx)
		stop()
		cancel()
		if err != nil {
			if ctx.Err() != nil {
				return 0, ctx.Err()
			}
			if t.failCtx.Err() != nil {
				continue
			}
			return 0, err
		}
	}
}

// poll converts the completions in the queue; a failed one breaks the
// transport and is dropped.
func (t *rcTransport) poll(out []completion) (int, error) {
	n := 0
	for n < len(out) {
		k, err := t.cq.Poll(t.wcs[:min(len(t.wcs), len(out)-n)])
		if err != nil {
			return n, err
		}
		for _, wc := range t.wcs[:k] {
			if wc.Status != verbs.WCSuccess {
				t.fail(wcError(wc))
				continue
			}
			if wc.ID&sendIDFlag != 0 {
				out[n] = completion{send: true, slot: int(wc.ID &^ sendIDFlag)}
			} else {
				out[n] = completion{slot: int(wc.ID), n: int(wc.Len)}
			}
			n++
		}
		if k == 0 {
			break
		}
	}
	return n, nil
}

// wcError maps a failed completion to the error a socket would report.
func wcError(wc verbs.WC) error {
	switch wc.Status {
	case verbs.WCWRFlushErr:
		return errConnReset
	case verbs.WCRetryExcErr, verbs.WCRNRRetryExcErr:
		return syscall.ETIMEDOUT
	}
	return wc.Err()
}

// disconnect sends the disconnect request and waits for the peer's answer
// until ctx ends; the watcher is stopped first so that the ID's events are
// ours to read.
func (t *rcTransport) disconnect(ctx context.Context) error {
	t.stopWatch()
	return t.doDisconnect(ctx)
}

func (t *rcTransport) doDisconnect(ctx context.Context) error {
	if !t.disconnected.CompareAndSwap(false, true) {
		return nil
	}
	ctx, cancel := context.WithTimeout(ctx, disconnectTimeout)
	defer cancel()
	return t.id.Disconnect(ctx)
}

// close destroys the queue pair and the ID, then releases the rest.
func (t *rcTransport) close() error {
	t.stopWatch()
	err := t.id.Close()
	return errors.Join(err, t.release())
}

// release frees everything but the ID, which owns the queue pair and must
// be closed first.
func (t *rcTransport) release() error {
	var errs []error
	if t.sendMR != nil {
		errs = append(errs, t.sendMR.Close())
	}
	if t.recvMR != nil {
		errs = append(errs, t.recvMR.Close())
	}
	if t.cq != nil {
		errs = append(errs, t.cq.Close())
	}
	if t.ch != nil {
		errs = append(errs, t.ch.Close())
	}
	if t.pd != nil {
		errs = append(errs, t.pd.Close())
	}
	registeredBytes.Add(-t.registered)
	t.registered = 0
	t.failCancel()
	return errors.Join(errs...)
}

func (t *rcTransport) localAddr() netip.AddrPort  { return t.id.LocalAddr() }
func (t *rcTransport) remoteAddr() netip.AddrPort { return t.id.RemoteAddr() }
