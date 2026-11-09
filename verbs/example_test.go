package verbs_test

import (
	"context"
	"fmt"
	"log"
	"time"

	"github.com/wzshiming/rdma/verbs"
)

// Example opens the first device, connects an RC queue pair to a peer whose
// identity was exchanged out of band, and receives one message through a
// completion channel.
func Example() {
	devs, err := verbs.Devices()
	if err != nil || len(devs) == 0 {
		log.Fatal("no RDMA device: ", err)
	}
	ctx, err := devs[0].Open()
	if err != nil {
		log.Fatal(err)
	}
	defer ctx.Close()

	pd, err := ctx.AllocPD()
	if err != nil {
		log.Fatal(err)
	}
	defer pd.Close()
	mr, err := pd.AllocMemory(4096, verbs.AccessLocalWrite|verbs.AccessRemoteWrite)
	if err != nil {
		log.Fatal(err)
	}
	defer mr.Close()

	ch, err := ctx.CreateCompChannel()
	if err != nil {
		log.Fatal(err)
	}
	defer ch.Close()
	cq, err := ctx.CreateCQ(16, ch)
	if err != nil {
		log.Fatal(err)
	}
	defer cq.Close()
	qp, err := pd.CreateQP(&verbs.QPInitAttr{
		SendCQ: cq,
		RecvCQ: cq,
		Type:   verbs.QPTypeRC,
		Cap:    verbs.QPCap{MaxSendWR: 16, MaxRecvWR: 16, MaxSendSGE: 1, MaxRecvSGE: 1},
	})
	if err != nil {
		log.Fatal(err)
	}
	defer qp.Close()

	// The peer's queue pair number and GID, plus the local GID index of the
	// RoCE v2 address to use, come from an out-of-band exchange (rdma_cm).
	const port, gidIndex = 1, 1
	var peerQPN uint32
	var peerGID verbs.GID
	steps := []struct {
		attr verbs.QPAttr
		mask verbs.QPAttrMask
	}{{
		attr: verbs.QPAttr{State: verbs.QPStateInit, Port: port, AccessFlags: verbs.AccessLocalWrite | verbs.AccessRemoteWrite},
		mask: verbs.QPAttrState | verbs.QPAttrPkeyIndex | verbs.QPAttrPort | verbs.QPAttrAccessFlags,
	}, {
		attr: verbs.QPAttr{
			State: verbs.QPStateRTR, PathMTU: verbs.MTU1024, DestQPNum: peerQPN, MaxDestRDAtomic: 1, MinRNRTimer: 12,
			AH: verbs.AHAttr{IsGlobal: true, Port: port, GRH: verbs.GlobalRoute{DGID: peerGID, SGIDIndex: gidIndex, HopLimit: 64}},
		},
		mask: verbs.QPAttrState | verbs.QPAttrAV | verbs.QPAttrPathMTU | verbs.QPAttrDestQPN | verbs.QPAttrRQPSN | verbs.QPAttrMaxDestRDAtomic | verbs.QPAttrMinRNRTimer,
	}, {
		attr: verbs.QPAttr{State: verbs.QPStateRTS, Timeout: 14, RetryCnt: 7, RNRRetry: 7, MaxRDAtomic: 1},
		mask: verbs.QPAttrState | verbs.QPAttrTimeout | verbs.QPAttrRetryCnt | verbs.QPAttrRNRRetry | verbs.QPAttrSQPSN | verbs.QPAttrMaxQPRDAtomic,
	}}
	for _, s := range steps {
		if err := qp.Modify(&s.attr, s.mask); err != nil {
			log.Fatal(err)
		}
	}

	if err := qp.PostRecv(verbs.RecvWR{ID: 1, SGE: []verbs.SGE{mr.SGE(mr.Bytes())}}); err != nil {
		log.Fatal(err)
	}
	if err := cq.RequestNotify(false); err != nil {
		log.Fatal(err)
	}
	wctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()
	if _, err := ch.Wait(wctx); err != nil {
		log.Fatal(err)
	}
	var wcs [1]verbs.WC
	n, err := cq.Poll(wcs[:])
	if err != nil {
		log.Fatal(err)
	}
	if n == 1 && wcs[0].Err() == nil {
		fmt.Printf("received %d bytes: %q\n", wcs[0].Len, mr.Bytes()[:wcs[0].Len])
	}
}
