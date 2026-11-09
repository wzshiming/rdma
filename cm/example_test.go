package cm_test

import (
	"context"
	"errors"
	"fmt"
	"log"
	"net/netip"
	"time"

	"github.com/wzshiming/rdma/cm"
	"github.com/wzshiming/rdma/verbs"
)

// createQP gives id a queue pair on a protection domain of its device.
func createQP(id *cm.ID) (*verbs.QP, error) {
	pd, err := id.Device().AllocPD()
	if err != nil {
		return nil, err
	}
	cq, err := id.Device().CreateCQ(16, nil)
	if err != nil {
		return nil, err
	}
	return id.CreateQP(pd, &verbs.QPInitAttr{
		SendCQ: cq,
		RecvCQ: cq,
		Type:   verbs.QPTypeRC,
		Cap:    verbs.QPCap{MaxSendWR: 16, MaxRecvWR: 16, MaxSendSGE: 1, MaxRecvSGE: 1},
	})
}

// Example connects to a listener, exchanges private data with it and
// disconnects.
func Example() {
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()
	id, err := cm.Resolve(ctx, netip.AddrPort{}, netip.MustParseAddrPort("10.200.0.1:4791"))
	if err != nil {
		log.Fatal(err)
	}
	defer id.Close()
	qp, err := createQP(id)
	if err != nil {
		log.Fatal(err)
	}

	info, err := id.Connect(ctx, &cm.ConnParam{PrivateData: []byte("hello")})
	var rejected *cm.RejectedError
	switch {
	case errors.As(err, &rejected):
		log.Fatalf("rejected: %q", rejected.PrivateData)
	case err != nil:
		log.Fatal(err)
	}
	fmt.Printf("QP %d connected from %v to %v, the peer says %q\n", qp.Num(), id.LocalAddr(), id.RemoteAddr(), info.PrivateData[:5])

	// Post work requests on qp, then disconnect; the peer's Wait returns
	// EventDisconnected.
	if err := id.Disconnect(ctx); err != nil {
		log.Fatal(err)
	}
}

// ExampleListen accepts one connection and waits for the peer to disconnect.
func ExampleListen() {
	ln, err := cm.Listen(netip.MustParseAddrPort("10.200.0.1:4791"), 8)
	if err != nil {
		log.Fatal(err)
	}
	defer ln.Close()

	ctx, cancel := context.WithTimeout(context.Background(), time.Minute)
	defer cancel()
	req, err := ln.Accept(ctx)
	if err != nil {
		log.Fatal(err)
	}
	defer req.ID.Close()
	if string(req.PrivateData[:5]) != "hello" {
		if err := req.Reject([]byte("who are you")); err != nil {
			log.Fatal(err)
		}
		return
	}
	if _, err := createQP(req.ID); err != nil {
		log.Fatal(err)
	}
	if err := req.Accept(ctx, &cm.ConnParam{PrivateData: []byte("welcome")}); err != nil {
		log.Fatal(err)
	}

	ev, err := req.ID.Wait(ctx)
	if err != nil {
		log.Fatal(err)
	}
	fmt.Println("peer:", ev) // DISCONNECTED
}
