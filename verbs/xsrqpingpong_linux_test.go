//go:build linux && !android && (amd64 || arm64 || loong64 || ppc64le || riscv64 || s390x)

package verbs_test

import (
	"bytes"
	"context"
	"encoding/hex"
	"fmt"
	"io"
	"math/rand/v2"
	"net"
	"net/netip"
	"os"
	"os/exec"
	"slices"
	"strconv"
	"strings"
	"testing"
	"time"

	"github.com/wzshiming/rdma/internal/rdmatest"
	"github.com/wzshiming/rdma/verbs"
)

// The exchange of rdma-core's ibv_xsrq_pingpong (libibverbs/examples/
// xsrq_pingpong.c), which the interop test plays against it with one client.
// Each side has, in an XRC domain of its own, an XRC SRQ receiving into a
// buffer of xsrqppSize bytes, an XRC receive queue pair and an XRC send queue
// pair. Over TCP the client writes its address of 66 bytes,
// "LLLL:RRRRRR:SSSSSS:PPPPPP:NNNNNN:<32 hex digits of the GID>\0" (LID, the
// numbers of the receive and the send queue pair, PSN and SRQ number), and
// reads the server's; each side connects its receive queue pair to the
// other's send queue pair and its send queue pair to the other's receive
// queue pair. The server sends first, each side sends again for every
// message that comes in until it has sent xsrqppIters, signalling the last
// only, and both stop once they have received xsrqppIters. The client then
// writes "END\0", which the server reads and writes back.
const (
	xsrqppMsgSize = 66
	xsrqppSize    = 4096
	xsrqppDepth   = 4
	xsrqppIters   = 100
	xsrqppTimeout = 60 * time.Second
)

// xsrqppEnd is the message that ends the exchange.
var xsrqppEnd = []byte("END\x00")

// xsrqppDest is the address of a side.
type xsrqppDest struct {
	lid                         uint16
	recvQPN, sendQPN, psn, srqn uint32
	gid                         verbs.GID
}

// wire returns the 66 bytes of d in the exchange: the NUL-terminated text of
// sprintf("%04x:%06x:%06x:%06x:%06x:%32s") with the GID's bytes in hex.
func (d xsrqppDest) wire() []byte {
	b := make([]byte, xsrqppMsgSize)
	copy(b, fmt.Sprintf("%04x:%06x:%06x:%06x:%06x:%s", d.lid, d.recvQPN, d.sendQPN, d.psn, d.srqn, hex.EncodeToString(d.gid[:])))
	return b
}

// parseXSRQPPDest reads an address the way sscanf("%x:%x:%x:%x:%x:%s")
// does.
func parseXSRQPPDest(b []byte) (xsrqppDest, error) {
	if i := bytes.IndexByte(b, 0); i >= 0 {
		b = b[:i]
	}
	f := strings.Split(string(b), ":")
	if len(f) != 6 {
		return xsrqppDest{}, fmt.Errorf("address %q has %d fields, want 6", b, len(f))
	}
	var n [5]uint64
	for i := range n {
		var err error
		if n[i], err = strconv.ParseUint(f[i], 16, 32); err != nil {
			return xsrqppDest{}, fmt.Errorf("address %q: %v", b, err)
		}
	}
	d := xsrqppDest{lid: uint16(n[0]), recvQPN: uint32(n[1]), sendQPN: uint32(n[2]), psn: uint32(n[3]), srqn: uint32(n[4])}
	g, err := hex.DecodeString(f[5])
	if err != nil || len(g) != len(d.gid) {
		return xsrqppDest{}, fmt.Errorf("address %q: bad GID", b)
	}
	copy(d.gid[:], g)
	return d, nil
}

// An address of the exchange is the text the C program's sprintf and
// gid_to_wire_gid make: LID, the numbers of the receive and the send queue
// pair, PSN and SRQ number in hex, then the GID's bytes in hex,
// NUL-terminated in 66 bytes.
func TestXSRQPingpongAddressText(t *testing.T) {
	d := xsrqppDest{lid: 0x12, recvQPN: 0x345, sendQPN: 0x6789a, psn: 0xabcdef, srqn: 0x1c, gid: verbs.GID{0: 0xfe, 1: 0x80, 8: 0x02, 15: 0x01}}
	want := "0012:000345:06789a:abcdef:00001c:fe800000000000000200000000000001\x00"
	if got := string(d.wire()); got != want {
		t.Errorf("wire() = %q, want %q", got, want)
	}
	if got, err := parseXSRQPPDest([]byte(want)); err != nil || got != d {
		t.Errorf("parseXSRQPPDest(%q) = %+v, %v; want %+v", want, got, err, d)
	}
}

// xsrqppSide is the Go side of the exchange.
type xsrqppSide struct {
	dev            rdmatest.Dev
	mr             *verbs.MR
	sendCQ, recvCQ *verbs.CQ
	srq            *verbs.SRQ
	recvQP, sendQP *verbs.QP
	local          xsrqppDest
	sent           int
}

// newXSRQPPSide sets up the Go side as pp_init_ctx and create_qps do, with
// the queue pairs in INIT and the SRQ's receives posted. On a device without
// XRC the test skips.
func newXSRQPPSide(t *testing.T) *xsrqppSide {
	t.Helper()
	dev, ctx := openDevice(t)
	xrcd := openXRCD(t, dev, ctx)
	s := &xsrqppSide{dev: dev}
	pd := allocPD(t, ctx)
	s.mr = allocMR(t, pd, xsrqppSize)
	s.sendCQ, s.recvCQ = createCQ(t, ctx, nil), createCQ(t, ctx, nil)
	var srqn uint32
	s.srq, srqn = createXRCSRQ(t, pd, xrcd, s.recvCQ, xsrqppDepth)
	s.recvQP = createXRCQP(t, pd, &verbs.QPInitAttr{Type: verbs.QPTypeXRCRecv, XRCD: xrcd})
	s.sendQP = createXRCQP(t, pd, &verbs.QPInitAttr{Type: verbs.QPTypeXRCSend, SendCQ: s.sendCQ, Cap: verbs.QPCap{MaxSendWR: xsrqppIters, MaxSendSGE: 1}})
	port := uint8(dev.Port)
	attr := verbs.QPAttr{State: verbs.QPStateInit, Port: port, AccessFlags: verbs.AccessRemoteWrite | verbs.AccessRemoteRead}
	mask := verbs.QPAttrState | verbs.QPAttrPkeyIndex | verbs.QPAttrPort | verbs.QPAttrAccessFlags
	if err := s.recvQP.Modify(&attr, mask); err != nil {
		t.Fatalf("Modify(XRC_RECV to INIT) error: %v", err)
	}
	attr.AccessFlags = 0
	if err := s.sendQP.Modify(&attr, mask); err != nil {
		t.Fatalf("Modify(XRC_SEND to INIT) error: %v", err)
	}
	pa, err := ctx.QueryPort(port)
	if err != nil {
		t.Fatalf("QueryPort(%d) error: %v", port, err)
	}
	gids, err := ctx.GIDTable(port)
	if err != nil {
		t.Fatalf("GIDTable(%d) error: %v", port, err)
	}
	var gid verbs.GID
	for _, g := range gids {
		if g.Index == dev.GIDIndex {
			gid = g.GID
		}
	}
	s.local = xsrqppDest{lid: pa.LID, recvQPN: s.recvQP.Num(), sendQPN: s.sendQP.Num(), psn: rand.Uint32() & 0xffffff, srqn: srqn, gid: gid}
	s.postRecvs(t, xsrqppDepth)
	return s
}

// postRecvs posts n receives of the whole buffer to the SRQ.
func (s *xsrqppSide) postRecvs(t *testing.T, n int) {
	t.Helper()
	wrs := make([]verbs.RecvWR, n)
	for i := range wrs {
		wrs[i] = verbs.RecvWR{ID: 1, SGE: []verbs.SGE{s.mr.SGE(s.mr.Bytes())}}
	}
	if err := s.srq.PostRecv(wrs...); err != nil {
		t.Fatalf("SRQ.PostRecv() of %d receives error: %v", n, err)
	}
}

// writeDest writes the side's address.
func (s *xsrqppSide) writeDest(t *testing.T, conn net.Conn) {
	t.Helper()
	if _, err := conn.Write(s.local.wire()); err != nil {
		t.Fatalf("writing the local address: %v", err)
	}
}

// readXSRQPPDest reads the peer's address.
func readXSRQPPDest(t *testing.T, conn net.Conn) xsrqppDest {
	t.Helper()
	b := make([]byte, xsrqppMsgSize)
	if _, err := io.ReadFull(conn, b); err != nil {
		t.Fatalf("reading the remote address: %v", err)
	}
	d, err := parseXSRQPPDest(b)
	if err != nil {
		t.Fatalf("remote address: %v", err)
	}
	return d
}

// readXSRQPPEnd reads the message that ends the exchange.
func readXSRQPPEnd(t *testing.T, conn net.Conn) {
	t.Helper()
	b := make([]byte, len(xsrqppEnd))
	if _, err := io.ReadFull(conn, b); err != nil || !bytes.Equal(b, xsrqppEnd) {
		t.Fatalf("reading END: %q, %v", b, err)
	}
}

// connect drives the queue pairs to RTS towards the peer's as connect_qps
// does, with a path MTU of 1024 and service level 0: the receive queue pair
// takes the peer's send queue pair for its destination, the send queue pair
// the peer's receive queue pair.
func (s *xsrqppSide) connect(t *testing.T, peer xsrqppDest) {
	t.Helper()
	t.Logf("local address %+v, remote %+v", s.local, peer)
	ah := verbs.AHAttr{DLID: peer.lid, Port: uint8(s.dev.Port)}
	// As the C program does, the peer's GID decides whether the path is global.
	if !slices.Equal(peer.gid[8:], make([]byte, 8)) {
		ah.IsGlobal = true
		ah.GRH = verbs.GlobalRoute{DGID: peer.gid, SGIDIndex: uint8(s.dev.GIDIndex), HopLimit: 5}
	}
	rtr := verbs.QPAttr{State: verbs.QPStateRTR, PathMTU: verbs.MTU1024, DestQPNum: peer.sendQPN, RQPSN: peer.psn, MinRNRTimer: 12, AH: ah}
	err := s.recvQP.Modify(&rtr, verbs.QPAttrState|verbs.QPAttrAV|verbs.QPAttrPathMTU|verbs.QPAttrDestQPN|verbs.QPAttrRQPSN|verbs.QPAttrMaxDestRDAtomic|verbs.QPAttrMinRNRTimer)
	if err != nil {
		t.Fatalf("Modify(XRC_RECV to RTR) error: %v", err)
	}
	rts := verbs.QPAttr{State: verbs.QPStateRTS, Timeout: 14, SQPSN: s.local.psn}
	if err := s.recvQP.Modify(&rts, verbs.QPAttrState|verbs.QPAttrTimeout|verbs.QPAttrSQPSN); err != nil {
		t.Fatalf("Modify(XRC_RECV to RTS) error: %v", err)
	}
	rtr = verbs.QPAttr{State: verbs.QPStateRTR, PathMTU: verbs.MTU1024, DestQPNum: peer.recvQPN, RQPSN: peer.psn, AH: ah}
	if err := s.sendQP.Modify(&rtr, verbs.QPAttrState|verbs.QPAttrAV|verbs.QPAttrPathMTU|verbs.QPAttrDestQPN|verbs.QPAttrRQPSN); err != nil {
		t.Fatalf("Modify(XRC_SEND to RTR) error: %v", err)
	}
	rts = verbs.QPAttr{State: verbs.QPStateRTS, Timeout: 14, RetryCnt: 7, RNRRetry: 7, SQPSN: s.local.psn}
	err = s.sendQP.Modify(&rts, verbs.QPAttrState|verbs.QPAttrTimeout|verbs.QPAttrSQPSN|verbs.QPAttrRetryCnt|verbs.QPAttrRNRRetry|verbs.QPAttrMaxQPRDAtomic)
	if err != nil {
		t.Fatalf("Modify(XRC_SEND to RTS) error: %v", err)
	}
}

// postSend sends the buffer to the peer's SRQ, signalled if it is the last.
func (s *xsrqppSide) postSend(t *testing.T, peer xsrqppDest) {
	t.Helper()
	s.sent++
	var flags verbs.SendFlags
	if s.sent == xsrqppIters {
		flags = verbs.SendSignaled
	}
	err := s.sendQP.PostSend(verbs.SendWR{ID: uint64(s.sent), Op: verbs.WRSend, Flags: flags, SGE: []verbs.SGE{s.mr.SGE(s.mr.Bytes())}, RemoteSRQN: peer.srqn})
	if err != nil {
		t.Fatalf("PostSend() %d to SRQ %#x error: %v", s.sent, peer.srqn, err)
	}
}

// pingpong runs the loop of the C program's main for one client and returns
// once xsrqppIters messages have come in and the last send, the only one
// signalled, has completed.
func (s *xsrqppSide) pingpong(t *testing.T, peer xsrqppDest, server bool) {
	t.Helper()
	if server {
		s.postSend(t, peer)
	}
	wcs := make([]verbs.WC, xsrqppDepth)
	deadline := time.Now().Add(xsrqppTimeout)
	for received := 0; received < xsrqppIters; {
		if time.Now().After(deadline) {
			t.Fatalf("after %v: %d of %d messages received, %d sent", xsrqppTimeout, received, xsrqppIters, s.sent)
		}
		n, err := s.recvCQ.Poll(wcs)
		if err != nil {
			t.Fatalf("Poll() error: %v", err)
		}
		if n == 0 {
			time.Sleep(50 * time.Microsecond)
		}
		for _, wc := range wcs[:n] {
			if err := wc.Err(); err != nil {
				t.Fatalf("receive %d failed: %v", received+1, err)
			}
			if wc.Opcode != verbs.WCOpRecv || wc.QPNum != s.recvQP.Num() || wc.Len != xsrqppSize {
				t.Fatalf("completion %+v, want a receive of %d bytes on XRC receive queue pair %#x", wc, xsrqppSize, s.recvQP.Num())
			}
			s.postRecvs(t, 1)
			if s.sent < xsrqppIters {
				s.postSend(t, peer)
			}
			received++
		}
	}
	if wc := pollOne(t, s.sendCQ); wc.ID != xsrqppIters {
		t.Errorf("send completion %+v, want that of send %d, the last", wc, xsrqppIters)
	}
}

// xsrqPingpong finds ibv_xsrq_pingpong on PATH, skipping the test without
// it, or failing it under RDMA_TEST_REQUIRE=1.
func xsrqPingpong(t *testing.T) string {
	t.Helper()
	path, err := exec.LookPath("ibv_xsrq_pingpong")
	if err != nil {
		msg := "ibv_xsrq_pingpong (rdma-core's libibverbs examples) not found: " + err.Error()
		if os.Getenv("RDMA_TEST_REQUIRE") == "1" {
			t.Fatal(msg + " (RDMA_TEST_REQUIRE=1)")
		}
		t.Skip(msg)
	}
	return path
}

// startXSRQPingpong runs ibv_xsrq_pingpong for the side's device with tcpPort
// and, for a client, the server's address, and returns a function that waits
// for it and checks that it exited 0 after printing success. Stopped before
// that, it leaves its output in the log.
func startXSRQPingpong(t *testing.T, s *xsrqppSide, tcpPort int, server netip.Addr) (wait func()) {
	t.Helper()
	path := xsrqPingpong(t)
	args := []string{
		"-d", s.dev.Name, "-i", strconv.Itoa(s.dev.Port), "-g", strconv.Itoa(s.dev.GIDIndex),
		"-p", strconv.Itoa(tcpPort), "-s", strconv.Itoa(xsrqppSize), "-n", strconv.Itoa(xsrqppIters), "-m", "1024",
	}
	if server.IsValid() {
		args = append(args, server.String())
	}
	ctx, cancel := context.WithTimeout(context.Background(), xsrqppTimeout)
	cmd := exec.CommandContext(ctx, path, args...)
	var stdout, stderr bytes.Buffer
	cmd.Stdout, cmd.Stderr = &stdout, &stderr
	if err := cmd.Start(); err != nil {
		cancel()
		t.Fatalf("start %s: %v", path, err)
	}
	waited := false
	t.Cleanup(func() {
		cancel()
		if !waited {
			err := cmd.Wait()
			t.Logf("ibv_xsrq_pingpong %s stopped with %v\nstdout:\n%sstderr:\n%s", strings.Join(args, " "), err, stdout.String(), stderr.String())
		}
	})
	return func() {
		t.Helper()
		waited = true
		err := cmd.Wait()
		cancel()
		t.Logf("ibv_xsrq_pingpong %s exited with %v\nstdout:\n%sstderr:\n%s", strings.Join(args, " "), err, stdout.String(), stderr.String())
		if err != nil {
			t.Errorf("ibv_xsrq_pingpong %s: %v", strings.Join(args, " "), err)
		}
		if !strings.Contains(stdout.String(), "success") {
			t.Errorf("ibv_xsrq_pingpong %s did not print success", strings.Join(args, " "))
		}
	}
}

// Go and rdma-core's ibv_xsrq_pingpong send messages to each other's XRC SRQ
// through XRC queue pairs, with Go as the server and as the client. On a
// device without XRC the test skips.
func TestXSRQPingpongInterop(t *testing.T) {
	t.Run("ibv_xsrq_pingpong client", func(t *testing.T) {
		s := newXSRQPPSide(t)
		ln, err := net.Listen("tcp", net.JoinHostPort(s.dev.IP.String(), "0"))
		if err != nil {
			t.Fatalf("Listen() error: %v", err)
		}
		defer ln.Close()
		wait := startXSRQPingpong(t, s, ln.Addr().(*net.TCPAddr).Port, s.dev.IP)
		ln.(*net.TCPListener).SetDeadline(time.Now().Add(xsrqppTimeout))
		conn, err := ln.Accept()
		if err != nil {
			t.Fatalf("Accept() error: %v", err)
		}
		defer conn.Close()
		conn.SetDeadline(time.Now().Add(xsrqppTimeout))
		peer := readXSRQPPDest(t, conn)
		s.writeDest(t, conn)
		s.connect(t, peer)
		s.pingpong(t, peer, true)
		readXSRQPPEnd(t, conn)
		if _, err := conn.Write(xsrqppEnd); err != nil {
			t.Fatalf("writing END: %v", err)
		}
		wait()
	})
	t.Run("ibv_xsrq_pingpong server", func(t *testing.T) {
		s := newXSRQPPSide(t)
		ln, err := net.Listen("tcp", ":0")
		if err != nil {
			t.Fatalf("Listen() error: %v", err)
		}
		port := ln.Addr().(*net.TCPAddr).Port
		ln.Close()
		wait := startXSRQPingpong(t, s, port, netip.Addr{})
		var conn net.Conn
		for deadline := time.Now().Add(10 * time.Second); ; time.Sleep(50 * time.Millisecond) {
			if conn, err = net.Dial("tcp", net.JoinHostPort(s.dev.IP.String(), strconv.Itoa(port))); err == nil || time.Now().After(deadline) {
				break
			}
		}
		if err != nil {
			t.Fatalf("Dial() to ibv_xsrq_pingpong error: %v", err)
		}
		defer conn.Close()
		conn.SetDeadline(time.Now().Add(xsrqppTimeout))
		s.writeDest(t, conn)
		peer := readXSRQPPDest(t, conn)
		s.connect(t, peer)
		s.pingpong(t, peer, false)
		if _, err := conn.Write(xsrqppEnd); err != nil {
			t.Fatalf("writing END: %v", err)
		}
		readXSRQPPEnd(t, conn)
		wait()
	})
}
