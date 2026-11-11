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
// up: one completion queue for both directions, on a completion channel of
// its own. Two goroutines drive it, parked on the runtime poller while idle:
// the pump feeds the stream the work completions and the watcher waits for
// the connection to end. Only Linux has the verbs and cm to run it; elsewhere
// they report errors.ErrUnsupported.
type qpTransport struct {
	id     *cm.ID
	pd     *verbs.PD
	ch     *verbs.CompChannel
	cq     *verbs.CQ
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

	wcs     [32]verbs.WC // the pump's
	lossFed bool         // the pump's: the loss of the link went to the stream
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
	if t.cq, err = dev.CreateCQ(sz.sendQueue+sz.receiveQueue, t.ch); err != nil {
		return err
	}
	qa := verbs.QPInitAttr{
		SendCQ: t.cq,
		RecvCQ: t.cq,
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
	go t.pump(ev)
	go t.watch()
}

// pump feeds ev the work completions until down, and the loss of the link
// once the watcher has seen the connection end and the completions before
// that are fed.
func (t *qpTransport) pump(ev linkEvents) {
	defer close(t.pumped)
	for t.ctx.Err() == nil {
		if err := t.step(ev); err != nil {
			t.fail(ev, err)
			return
		}
	}
}

// step feeds ev what the completion queue holds, arms the completion channel
// and polls once more, a completion before the arm raising no event, and
// sleeps on the channel when that finds nothing: until the connection ends,
// while its loss is yet to be fed, and until down after that.
func (t *qpTransport) step(ev linkEvents) error {
	if _, err := t.drain(ev); err != nil {
		return err
	}
	if err := t.cq.RequestNotify(false); err != nil {
		return err
	}
	if n, err := t.drain(ev); n > 0 || err != nil {
		return err
	}
	sleep := t.lossCtx
	if t.lossFed {
		sleep = t.ctx
	}
	if _, err := t.ch.Wait(sleep); err != nil && sleep.Err() == nil {
		return err
	}
	return nil
}

// drain feeds ev every completion in the queue, a batch at a time under ev's
// lock, and returns how many there were. Once the watcher has seen the
// connection end, the first drain to empty the queue feeds the loss of the
// link after them: what the queue held then came before the end.
func (t *qpTransport) drain(ev linkEvents) (int, error) {
	var loss error
	if !t.lossFed && t.ctx.Err() == nil && t.lossCtx.Err() != nil {
		loss = context.Cause(t.lossCtx)
	}
	total := 0
	for {
		n, err := t.cq.Poll(t.wcs[:])
		if err != nil {
			return total, err
		}
		total += n
		empty := n < len(t.wcs)
		if n > 0 || empty && loss != nil {
			ev.lock()
			for i := range t.wcs[:n] {
				t.feed(ev, &t.wcs[i])
			}
			if empty && loss != nil {
				ev.lost(loss)
				t.lossFed = true
			}
			ev.unlock()
		}
		if empty {
			return total, nil
		}
	}
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

// fail loses the link to err, a failure of the completion queue or its
// channel, unless it is lost already, and waits for down.
func (t *qpTransport) fail(ev linkEvents, err error) {
	if !t.lossFed {
		ev.lock()
		ev.lost(err)
		ev.unlock()
		t.lossFed = true
	}
	<-t.ctx.Done()
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

// down stops the goroutines, disconnects, which moves the queue pair to the
// error state and tells the peer, and destroys the queue pair and id.
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
	if t.cq != nil {
		t.cq.Close()
	}
	if t.ch != nil {
		t.ch.Close()
	}
	if t.pd != nil {
		t.pd.Close()
	}
}
