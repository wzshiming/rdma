//go:build linux && !android && (amd64 || arm64 || loong64 || ppc64le || riscv64 || s390x)

package verbs_test

import (
	"context"
	"errors"
	"testing"
	"time"

	"github.com/wzshiming/rdma/verbs"
)

// createSRQ creates a shared receive queue for maxWR receives of one SGE on
// pd, closed with the test, and returns it with the attributes granted.
func createSRQ(t *testing.T, pd *verbs.PD, maxWR uint32) (*verbs.SRQ, verbs.SRQAttr) {
	t.Helper()
	attr := verbs.SRQInitAttr{Attr: verbs.SRQAttr{MaxWR: maxWR, MaxSGE: 1}}
	srq, err := pd.CreateSRQ(&attr)
	if err != nil {
		t.Fatalf("CreateSRQ() error: %v", err)
	}
	t.Cleanup(func() {
		if err := srq.Close(); err != nil {
			t.Errorf("SRQ.Close() error: %v", err)
		}
	})
	if attr.Attr.MaxWR < maxWR || attr.Attr.MaxSGE < 1 {
		t.Fatalf("CreateSRQ() granted %+v, want at least %d receives of 1 SGE", attr.Attr, maxWR)
	}
	return srq, attr.Attr
}

// createSRQRC creates an RC queue pair on pd that completes on cq and
// receives through srq.
func createSRQRC(t *testing.T, pd *verbs.PD, cq *verbs.CQ, srq *verbs.SRQ) *verbs.QP {
	t.Helper()
	qp, err := pd.CreateQP(&verbs.QPInitAttr{
		SendCQ: cq,
		RecvCQ: cq,
		SRQ:    srq,
		Type:   verbs.QPTypeRC,
		Cap:    verbs.QPCap{MaxSendWR: 8, MaxSendSGE: 1},
	})
	if err != nil {
		t.Fatalf("CreateQP() with an SRQ error: %v", err)
	}
	t.Cleanup(func() {
		if err := qp.Close(); err != nil {
			t.Errorf("QP.Close() error: %v", err)
		}
	})
	return qp
}

// srqPair is RC queue pairs on the rdmatest device whose receivers share one
// SRQ: senders[i] is connected to receivers[i], the senders complete on
// sendCQ and the receivers on recvCQ.
type srqPair struct {
	*rcPair
	sendCQ, recvCQ *verbs.CQ
	srq            *verbs.SRQ
	granted        verbs.SRQAttr
	senders        []*verbs.QP
	receivers      []*verbs.QP
	mr             *verbs.MR
}

func newSRQPair(t *testing.T, n int) *srqPair {
	t.Helper()
	dev, ctx := openDevice(t)
	p := &srqPair{rcPair: &rcPair{dev: dev, ctx: ctx, pd: allocPD(t, ctx)}}
	p.sendCQ, p.recvCQ = createCQ(t, ctx, nil), createCQ(t, ctx, nil)
	p.srq, p.granted = createSRQ(t, p.pd, 16)
	for range n {
		s, _ := createRC(t, p.pd, p.sendCQ)
		r := createSRQRC(t, p.pd, p.recvCQ, p.srq)
		p.connect(t, s, r)
		p.connect(t, r, s)
		p.senders, p.receivers = append(p.senders, s), append(p.receivers, r)
	}
	p.mr = allocMR(t, p.pd, 8192)
	return p
}

// slot returns the 64-byte receive buffer i of the pair's memory.
func (p *srqPair) slot(i uint64) []byte { return p.mr.Bytes()[i*64 : i*64+64] }

// postRecvs posts n receives with IDs from to from+n-1 to the SRQ, each into
// the slot of its ID.
func (p *srqPair) postRecvs(t *testing.T, from uint64, n int) {
	t.Helper()
	wrs := make([]verbs.RecvWR, n)
	for i := range wrs {
		id := from + uint64(i)
		wrs[i] = verbs.RecvWR{ID: id, SGE: []verbs.SGE{p.mr.SGE(p.slot(id))}}
	}
	if err := p.srq.PostRecv(wrs...); err != nil {
		t.Fatalf("SRQ.PostRecv() error: %v", err)
	}
}

// send sends msg from sender i and waits for its completion.
func (p *srqPair) send(t *testing.T, i int, msg string) {
	t.Helper()
	b := p.mr.Bytes()[4096+i*64:]
	copy(b, msg)
	err := p.senders[i].PostSend(verbs.SendWR{ID: uint64(1000 + i), Op: verbs.WRSend, Flags: verbs.SendSignaled, SGE: []verbs.SGE{p.mr.SGE(b[:len(msg)])}})
	if err != nil {
		t.Fatalf("PostSend() from sender %d error: %v", i, err)
	}
	pollOne(t, p.sendCQ)
}

// Two RC queue pairs that share one SRQ receive through it: each message
// lands in the buffer of a receive posted to the SRQ, and its completion
// names the queue pair it came in on.
func TestSRQSharedByRCQueuePairs(t *testing.T) {
	p := newSRQPair(t, 2)
	p.postRecvs(t, 0, 4)
	p.send(t, 0, "to the first receiver")
	p.send(t, 1, "to the second receiver")
	got := map[uint32]string{}
	ids := map[uint64]bool{}
	for _, wc := range pollN(t, p.recvCQ, 2) {
		if wc.Opcode != verbs.WCOpRecv || wc.ID > 3 {
			t.Fatalf("completion %+v, want a receive of one of the 4 posted", wc)
		}
		got[wc.QPNum] = string(p.slot(wc.ID)[:wc.Len])
		ids[wc.ID] = true
	}
	want := map[uint32]string{p.receivers[0].Num(): "to the first receiver", p.receivers[1].Num(): "to the second receiver"}
	for qpn, msg := range want {
		if got[qpn] != msg {
			t.Errorf("queue pair %#x received %q, want %q (all: %v)", qpn, got[qpn], msg, got)
		}
	}
	if !ids[0] || !ids[1] {
		t.Errorf("the messages used receives %v, want the first two posted, 0 and 1", ids)
	}
}

// The limit of a shared receive queue arms an event: once fewer receives
// than the limit are left, the device raises SRQ_LIMIT_REACHED for the queue
// and the limit is 0 again. Query reports the attributes at each step.
func TestSRQLimitEvent(t *testing.T) {
	p := newSRQPair(t, 1)
	p.postRecvs(t, 0, 8)
	if got, err := p.srq.Query(); err != nil || *got != p.granted {
		t.Fatalf("Query() = %+v, %v; want %+v, what CreateSRQ granted", got, err, p.granted)
	}
	if err := p.srq.Modify(&verbs.SRQAttr{Limit: 4}, verbs.SRQAttrLimit); err != nil {
		t.Fatalf("Modify(limit 4) error: %v", err)
	}
	if got, err := p.srq.Query(); err != nil || got.Limit != 4 || got.MaxWR != p.granted.MaxWR {
		t.Fatalf("Query() after arming = %+v, %v; want limit 4 and max_wr %d", got, err, p.granted.MaxWR)
	}
	for i := range 4 {
		p.send(t, 0, "message "+string(rune('a'+i)))
	}
	pollN(t, p.recvCQ, 4)
	idle, cancelIdle := context.WithTimeout(context.Background(), 200*time.Millisecond)
	defer cancelIdle()
	if ev, err := p.ctx.WaitAsyncEvent(idle); !errors.Is(err, context.DeadlineExceeded) {
		t.Fatalf("WaitAsyncEvent() with 4 receives left = %+v, %v; want none before fewer than 4 are left", ev, err)
	}
	p.send(t, 0, "message e")
	pollOne(t, p.recvCQ)
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()
	ev, err := p.ctx.WaitAsyncEvent(ctx)
	if err != nil {
		t.Fatalf("WaitAsyncEvent() error: %v", err)
	}
	if ev.Type != verbs.EventSRQLimitReached || ev.SRQ != p.srq {
		t.Errorf("WaitAsyncEvent() = {Type %v, SRQ %p}, want SRQ_LIMIT_REACHED on SRQ %p", ev.Type, ev.SRQ, p.srq)
	}
	if got, err := p.srq.Query(); err != nil || got.Limit != 0 {
		t.Errorf("Query() after the event = %+v, %v; want limit 0", got, err)
	}
}

// deviceCapSRQResize is IBV_DEVICE_SRQ_RESIZE of DeviceAttr.DeviceCapFlags.
const deviceCapSRQResize = 1 << 13

// Modify resizes a shared receive queue on a device that can, as rxe can.
func TestSRQResize(t *testing.T) {
	dev, ctx := openDevice(t)
	attr, err := ctx.QueryDevice()
	if err != nil {
		t.Fatalf("QueryDevice() error: %v", err)
	}
	if attr.DeviceCapFlags&deviceCapSRQResize == 0 {
		t.Skipf("%s cannot resize an SRQ", dev.Name)
	}
	srq, granted := createSRQ(t, allocPD(t, ctx), 16)
	want := granted.MaxWR + 16
	if err := srq.Modify(&verbs.SRQAttr{MaxWR: want}, verbs.SRQAttrMaxWR); err != nil {
		t.Fatalf("Modify(max_wr %d) error: %v", want, err)
	}
	if got, err := srq.Query(); err != nil || got.MaxWR < want {
		t.Errorf("Query() after the resize = %+v, %v; want max_wr at least %d", got, err, want)
	}
}
