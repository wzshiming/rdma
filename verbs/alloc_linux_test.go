//go:build linux && !android && !race && (amd64 || arm64 || loong64 || ppc64le || riscv64 || s390x)

package verbs_test

import (
	"testing"

	"github.com/wzshiming/rdma/verbs"
)

// The cgo binding allocated 1, 1 and 0 times per call on rxe (Go run
// 42851838307 of commit f481f61); the run-time loaded one builds the chain
// in the queue pair's own buffer and allocates nothing. Under the race
// detector sync.Pool, which dynlib draws its argument block from, drops
// entries at random, so these tests stay out of race builds.
const (
	maxPostSendAllocs = 0
	maxPostRecvAllocs = 0
	maxPollAllocs     = 0
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
	if n > maxPostSendAllocs {
		t.Errorf("PostSend() allocates %v times per call, want at most %d", n, maxPostSendAllocs)
	}
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
	if n > maxPostRecvAllocs {
		t.Errorf("PostRecv() allocates %v times per call, want at most %d", n, maxPostRecvAllocs)
	}
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
	if n > maxPollAllocs {
		t.Errorf("Poll() allocates %v times per call, want at most %d", n, maxPollAllocs)
	}
}
