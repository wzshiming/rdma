//go:build linux && cgo

package verbs_test

import (
	"testing"

	"github.com/wzshiming/rdma/verbs"
)

// allocRuns keeps the warm-up post and the measured ones within the 32-deep
// queues of newRCPair: the measured writes are unsignaled and the measured
// receives are never matched, so nothing drains the queues meanwhile.
const allocRuns = 20

func TestPostSendAllocations(t *testing.T) {
	p := newRCPair(t)
	mrA, mrB := allocMR(t, p.pd, 4096), allocMR(t, p.pd, 4096)
	wr := verbs.SendWR{ID: 1, Op: verbs.WRWrite, SGE: []verbs.SGE{mrA.SGE(mrA.Bytes()[:8])}, RemoteAddr: mrB.SGE(mrB.Bytes()).Addr, RKey: mrB.RKey()}
	n := testing.AllocsPerRun(allocRuns, func() {
		if err := p.a.PostSend(wr); err != nil {
			t.Fatalf("PostSend() error: %v", err)
		}
	})
	t.Logf("PostSend: %v allocations per call", n)
	// A signaled write behind the measured ones completes after all of them.
	wr.ID, wr.Flags = 2, verbs.SendSignaled
	if err := p.a.PostSend(wr); err != nil {
		t.Fatalf("PostSend(signaled) error: %v", err)
	}
	if wc := pollOne(t, p.cqA); wc.ID != 2 {
		t.Errorf("completion = %+v, want id 2", wc)
	}
}

func TestPostRecvAllocations(t *testing.T) {
	p := newRCPair(t)
	mr := allocMR(t, p.pd, 4096)
	wr := verbs.RecvWR{ID: 1, SGE: []verbs.SGE{mr.SGE(mr.Bytes()[:64])}}
	n := testing.AllocsPerRun(allocRuns, func() {
		if err := p.b.PostRecv(wr); err != nil {
			t.Fatalf("PostRecv() error: %v", err)
		}
	})
	t.Logf("PostRecv: %v allocations per call", n)
}

func TestPollAllocations(t *testing.T) {
	p := newRCPair(t)
	wcs := make([]verbs.WC, 4)
	n := testing.AllocsPerRun(100, func() {
		if k, err := p.cqA.Poll(wcs); err != nil || k != 0 {
			t.Fatalf("Poll() = %d, %v; want 0, nil", k, err)
		}
	})
	t.Logf("Poll: %v allocations per call", n)
}
