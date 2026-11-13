package rdma

import (
	"context"
	"encoding/binary"
	"fmt"

	"github.com/wzshiming/rdma/cm"
	"github.com/wzshiming/rdma/internal/rsproto"
	"github.com/wzshiming/rdma/verbs"
)

// recvWR marks the work requests of receives, whose completions it tells
// from those of sends: their ids are message words, of 32 bits.
const recvWR = 1 << 63

// bigEndian reports whether the host is big-endian, which the connection
// data tells the peer.
var bigEndian = binary.NativeEndian.Uint16([]byte{0, 1}) == 1

// qpTransport is the transport of a native stream on the RC queue pair of a
// connection that cm makes, with the memory of the stream registered on the
// device of the connection, as rs_create_ep and rs_init_bufs set an rsocket
// up, but for the completions: those of receives and those of sends go to
// queues of their own, where rsockets has one, both on a completion channel
// of the transport's own. The calls of the stream process the queues, and
// two goroutines, parked on the runtime poller while idle, drive the rest:
// the pump wakes for the events of the channel, which a queue raises only
// once armed for what the stream waits for, the receive queue for arrivals
// and the send queue for the completions of sends, and for the end of the
// connection, which the watcher waits for. Only Linux has the verbs and cm
// to run it; elsewhere they report errors.ErrUnsupported.
type qpTransport struct {
	id     *cm.ID
	pd     *verbs.PD
	ch     *verbs.CompChannel
	rcq    *verbs.CQ // the receives'
	scq    *verbs.CQ // the sends'
	qp     *verbs.QP // id's
	smr    *verbs.MR // the send buffer, then the control slots
	gmr    *verbs.MR // the grant table, which the peer writes
	rmr    *verbs.MR // the receive buffer, which the peer writes
	inline int       // the largest send the queue pair takes inline
	mem    rsproto.Config

	sgl [2]verbs.SGE // of a post, under the stream's lock as every post is

	ctx     context.Context // ends with down
	cancel  context.CancelFunc
	lossCtx context.Context // ends with down, or with the connection, the cause its loss
	lose    context.CancelCauseFunc
	started bool
	pumped  chan struct{} // closed as the pump returns
	watched chan struct{} // closed as the watcher returns

	proc    processor
	rwcs    [32]verbs.WC // the processor's: a batch of the receive queue
	swcs    [32]verbs.WC // the processor's: a batch of the send queue
	lossFed bool         // the processor's: the loss of the link went to the stream
	failed  bool         // the processor's: a queue failed and both are left alone
}

// newQPTransport sets a connection up on id, whose route is resolved, for a
// stream sized for c: a queue pair created through id, and the memory of the
// stream registered on the device. The transport owns id from then on; when
// it fails, it closes id.
func newQPTransport(id *cm.ID, c Config) (*qpTransport, error) {
	t := &qpTransport{id: id}
	if err := t.setUp(c); err != nil {
		id.Close()
		t.release()
		return nil, err
	}
	t.ctx, t.cancel = context.WithCancel(context.Background())
	t.lossCtx, t.lose = context.WithCancelCause(t.ctx)
	return t, nil
}

// setUp makes what the connection needs, as rs_create_ep does, and leaves
// what it made to release when it fails.
func (t *qpTransport) setUp(c Config) error {
	dev := t.id.Device()
	attr, err := dev.QueryDevice()
	if err != nil {
		return err
	}
	sz, err := sizesFor(c, attr.MaxQPWR)
	if err != nil {
		return err
	}
	if t.pd, err = dev.AllocPD(); err != nil {
		return err
	}
	if t.ch, err = dev.CreateCompChannel(); err != nil {
		return err
	}
	// Each queue holds as many completions as its work requests: no more
	// can wait to be polled.
	if t.rcq, err = dev.CreateCQ(sz.receiveQueue, t.ch); err != nil {
		return err
	}
	if t.scq, err = dev.CreateCQ(sz.sendQueue, t.ch); err != nil {
		return err
	}
	qa := verbs.QPInitAttr{
		SendCQ: t.scq,
		RecvCQ: t.rcq,
		Type:   verbs.QPTypeRC,
		Cap: verbs.QPCap{
			MaxSendWR:     uint32(sz.sendQueue),
			MaxRecvWR:     uint32(sz.receiveQueue),
			MaxSendSGE:    2,
			MaxRecvSGE:    1,
			MaxInlineData: uint32(sz.inline),
		},
		SignalAll: true,
	}
	if t.qp, err = t.id.CreateQP(t.pd, &qa); err != nil {
		return err
	}
	t.inline = int(qa.Cap.MaxInlineData)
	// rs_init_bufs adds the control slots behind the send buffer when grants
	// cannot go inline; they are always there here.
	const ctrlSlots, grantTable = rsproto.QPCtrlSize * rsproto.SGESize, rsproto.SGLSize * rsproto.SGESize
	if t.smr, err = t.pd.AllocMemory(sz.sendBuffer+ctrlSlots, verbs.AccessLocalWrite); err != nil {
		return err
	}
	written := verbs.AccessLocalWrite | verbs.AccessRemoteWrite
	if t.gmr, err = t.pd.AllocMemory(grantTable, written); err != nil {
		return err
	}
	if t.rmr, err = t.pd.AllocMemory(sz.receiveBuffer, written); err != nil {
		return err
	}
	t.mem = rsproto.Config{
		SendBuffer:       region(t.smr, 0, sz.sendBuffer, t.smr.LKey()),
		ControlBuffer:    region(t.smr, sz.sendBuffer, ctrlSlots, t.smr.LKey()),
		GrantTable:       region(t.gmr, 0, grantTable, t.gmr.RKey()),
		ReceiveBuffer:    region(t.rmr, 0, sz.receiveBuffer, t.rmr.RKey()),
		SendQueueSize:    sz.sendQueue,
		ReceiveQueueSize: sz.receiveQueue,
		BigEndian:        bigEndian,
	}
	return nil
}

// region returns the n bytes of mr at off with the address the link and the
// peer know them by, and key.
func region(mr *verbs.MR, off, n int, key uint32) rsproto.Region {
	b := mr.Bytes()[off : off+n : off+n]
	return rsproto.Region{Buf: b, Addr: mr.SGE(b).Addr, Key: key}
}

// PostWriteMsg posts the write with immediate data of rs_post_write_msg,
// inline when it fits.
func (t *qpTransport) PostWriteMsg(id uint64, sgl []rsproto.Segment, msg rsproto.Msg, addr uint64, key uint32) error {
	n := 0
	for i, s := range sgl {
		t.sgl[i] = verbs.SGE{Addr: s.Addr, Length: s.Length, LKey: s.Key}
		n += int(s.Length)
	}
	wr := verbs.SendWR{ID: id, Op: verbs.WRWriteImm, SGE: t.sgl[:len(sgl)], Imm: uint32(msg), RemoteAddr: addr, RKey: key}
	if n <= t.inline {
		wr.Flags = verbs.SendInline
	}
	return t.qp.PostSend(wr)
}

// PostMsg posts the write with immediate data and no bytes of rs_post_msg.
func (t *qpTransport) PostMsg(id uint64, msg rsproto.Msg) error {
	return t.qp.PostSend(verbs.SendWR{ID: id, Op: verbs.WRWriteImm, Imm: uint32(msg)})
}

// PostRecv posts a receive without a buffer, as rs_post_recv does.
func (t *qpTransport) PostRecv(id uint64) error {
	return t.qp.PostRecv(verbs.RecvWR{ID: id | recvWR})
}

func (t *qpTransport) start(ev linkEvents) {
	t.started = true
	t.pumped, t.watched = make(chan struct{}), make(chan struct{})
	t.proc.start(t, ev)
	// The first poll of each queue sets up what verbs polls it with, an
	// allocation the calls of the stream are spared.
	t.proc.process()
	go t.pump()
	go t.watch()
}

func (t *qpTransport) process() { t.proc.process() }

func (t *qpTransport) expect(k interest) { t.proc.expect(k) }

// pump has the queues processed as their events come, which arms the queue
// that raised one again while the stream still waits for its kind, and once
// the watcher has seen the connection end, so that its loss goes to the
// stream after the completions before it, until down.
func (t *qpTransport) pump() {
	defer close(t.pumped)
	sleep := t.lossCtx
	for {
		cq, err := t.ch.Wait(sleep)
		switch {
		case t.ctx.Err() != nil:
			return
		case err == nil && cq == t.scq:
			t.proc.spent(sendCompletions)
		case err == nil:
			t.proc.spent(arrivals)
		case sleep.Err() != nil:
			sleep = t.ctx
			t.proc.process()
		default:
			// The channel failed: the link is lost to that.
			t.lose(err)
			t.proc.process()
			<-t.ctx.Done()
			return
		}
	}
}

// drain feeds ev every completion in both queues, a batch at a time under
// ev's lock. A batch of sends is polled before the receives and fed after
// them, once the receive queue comes back short: what arrived before a send
// failed reaches ev before the failure. Once the watcher has seen the
// connection end, the first drain to find both queues short feeds the loss
// of the link after them: what they held then came before the end.
func (t *qpTransport) drain(ev linkEvents) (k interest, known bool) {
	if t.failed {
		return 0, false
	}
	var loss error
	if !t.lossFed && t.ctx.Err() == nil && t.lossCtx.Err() != nil {
		loss = context.Cause(t.lossCtx)
	}
	for {
		ns, err := t.scq.Poll(t.swcs[:])
		if err != nil {
			t.fail(ev, err)
			return 0, false
		}
		nr, err := t.rcq.Poll(t.rwcs[:])
		for err == nil && nr == len(t.rwcs) {
			k, known = t.feedBatch(ev, nr, 0, nil), true
			nr, err = t.rcq.Poll(t.rwcs[:])
		}
		if err != nil {
			t.fail(ev, err)
			return 0, false
		}
		// The receive queue, polled after the sends, came back short.
		empty := ns < len(t.swcs)
		var lost error
		if empty {
			lost = loss
		}
		if nr > 0 || ns > 0 || lost != nil {
			k, known = t.feedBatch(ev, nr, ns, lost), true
		}
		if empty {
			return k, known
		}
	}
}

// feedBatch feeds ev under its lock the first nr completions polled off the
// receive queue, then the first ns off the send queue, then loss unless it
// is nil, and returns what ev waits for then.
func (t *qpTransport) feedBatch(ev linkEvents, nr, ns int, loss error) interest {
	ev.lock()
	for i := range t.rwcs[:nr] {
		t.feed(ev, &t.rwcs[i])
	}
	for i := range t.swcs[:ns] {
		t.feed(ev, &t.swcs[i])
	}
	if loss != nil {
		ev.lost(loss)
		t.lossFed = true
	}
	k := ev.waitsFor()
	ev.unlock()
	return k
}

// arm has the queues of the kinds k raise an event for their next
// completion: the receive queue for arrivals, the send queue for the
// completions of sends.
func (t *qpTransport) arm(ev linkEvents, k interest) interest {
	if t.failed {
		return 0
	}
	for _, q := range [...]struct {
		kind interest
		cq   *verbs.CQ
	}{{arrivals, t.rcq}, {sendCompletions, t.scq}} {
		if k&q.kind == 0 {
			continue
		}
		if err := q.cq.RequestNotify(false); err != nil {
			t.fail(ev, err)
			return 0
		}
	}
	return k
}

// feed hands ev what wc reports: the end of a send, or the message word of a
// receive; a receive that failed, flushed as the queue pair went down, has
// none.
func (t *qpTransport) feed(ev linkEvents, wc *verbs.WC) {
	if wc.ID&recvWR == 0 {
		ev.sent(wc.ID, wc.Err())
		return
	}
	if wc.Status == verbs.WCSuccess && wc.HasImm() {
		ev.received(rsproto.Msg(wc.Imm))
	}
}

// fail leaves the queues alone for err, a failure of one of them, which the
// stream learns as the loss of the link unless it learned of one before.
func (t *qpTransport) fail(ev linkEvents, err error) {
	t.failed = true
	if !t.lossFed {
		ev.lock()
		ev.lost(err)
		ev.unlock()
		t.lossFed = true
	}
}

// watch waits for the end of the connection, which rdma_cm reports as the
// peer disconnects or the device goes away, and has the pump feed it as the
// loss of the link. It then answers the peer's disconnect, which moves the
// queue pair to the error state, so that the sends in flight complete at
// once rather than when the transport gives up on them.
func (t *qpTransport) watch() {
	defer close(t.watched)
	for {
		ev, err := t.id.Wait(t.ctx)
		if err != nil {
			return
		}
		if ev == cm.EventDisconnected || ev == cm.EventDeviceRemoval {
			t.lose(fmt.Errorf("rdma: connection ended: %v", ev))
			t.id.Disconnect(t.ctx)
			return
		}
	}
}

// down stops the goroutines and the processing, disconnects, which moves the
// queue pair to the error state and tells the peer, and destroys the queue
// pair and id.
func (t *qpTransport) down() {
	t.cancel()
	if t.started {
		<-t.watched
	}
	// The context has ended, so Disconnect does not wait for the peer.
	t.id.Disconnect(t.ctx)
	if t.started {
		<-t.pumped
	}
	t.proc.stop()
	t.id.Close()
}

// release frees the memory and the rest of what setUp made but the queue
// pair, which went with id.
func (t *qpTransport) release() {
	for _, mr := range [...]*verbs.MR{t.smr, t.gmr, t.rmr} {
		if mr != nil {
			mr.Close()
		}
	}
	for _, cq := range [...]*verbs.CQ{t.rcq, t.scq} {
		if cq != nil {
			cq.Close()
		}
	}
	if t.ch != nil {
		t.ch.Close()
	}
	if t.pd != nil {
		t.pd.Close()
	}
}
