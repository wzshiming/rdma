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

// The exchange of rdma-core's ibv_srq_pingpong (libibverbs/examples/
// srq_pingpong.c), which the interop test plays against it. Each side has
// srqppQPs RC queue pairs receiving through one SRQ of srqppDepth receives
// of srqppSize bytes. Over TCP the client writes srqppMaxQP addresses of 52
// bytes, "LLLL:QQQQQQ:PPPPPP:<32 hex digits of the GID>\0" (LID, queue pair
// number, PSN), used or not; the server reads them, connects its queue pairs,
// writes its own and reads "done\0", after which the client connects. The
// client then sends once on every queue pair; a side sends again on a queue
// pair once its last send there has completed and a message has come in on
// it, until it has sent srqppIters, and both stop once they have sent and
// received srqppIters messages each.
const (
	srqppMaxQP   = 256
	srqppMsgSize = 52
	srqppQPs     = 4
	srqppDepth   = 64
	srqppSize    = 4096
	srqppIters   = 200
	srqppTimeout = 60 * time.Second

	srqppRecvWRID = 1
	srqppSendWRID = 2
)

// srqppDest is the address of one queue pair of a side.
type srqppDest struct {
	lid      uint16
	qpn, psn uint32
	gid      verbs.GID
}

// wire returns the 52 bytes of d in the exchange: the NUL-terminated text of
// sprintf("%04x:%06x:%06x:%s") with the GID's bytes in hex.
func (d srqppDest) wire() []byte {
	b := make([]byte, srqppMsgSize)
	copy(b, fmt.Sprintf("%04x:%06x:%06x:%s", d.lid, d.qpn, d.psn, hex.EncodeToString(d.gid[:])))
	return b
}

// parseSRQPPDest reads an address the way sscanf("%x:%x:%x:%s") does.
func parseSRQPPDest(b []byte) (srqppDest, error) {
	if i := bytes.IndexByte(b, 0); i >= 0 {
		b = b[:i]
	}
	f := strings.Split(string(b), ":")
	if len(f) != 4 {
		return srqppDest{}, fmt.Errorf("address %q has %d fields, want 4", b, len(f))
	}
	var n [3]uint64
	for i := range n {
		var err error
		if n[i], err = strconv.ParseUint(f[i], 16, 32); err != nil {
			return srqppDest{}, fmt.Errorf("address %q: %v", b, err)
		}
	}
	var d srqppDest
	if g, err := hex.DecodeString(f[3]); err != nil || len(g) != len(d.gid) {
		return srqppDest{}, fmt.Errorf("address %q: bad GID", b)
	} else {
		copy(d.gid[:], g)
	}
	d.lid, d.qpn, d.psn = uint16(n[0]), uint32(n[1]), uint32(n[2])
	return d, nil
}

// An address of the exchange is the text the C program's sprintf and
// gid_to_wire_gid make: LID, queue pair number and PSN in hex, then the
// GID's bytes in hex, NUL-terminated in 52 bytes.
func TestSRQPingpongAddressText(t *testing.T) {
	d := srqppDest{lid: 0x12, qpn: 0x345, psn: 0xabcdef, gid: verbs.GID{10: 0xff, 11: 0xff, 12: 10, 13: 200, 15: 1}}
	want := "0012:000345:abcdef:00000000000000000000ffff0ac80001\x00"
	if got := string(d.wire()); got != want {
		t.Errorf("wire() = %q, want %q", got, want)
	}
	if got, err := parseSRQPPDest([]byte(want)); err != nil || got != d {
		t.Errorf("parseSRQPPDest(%q) = %+v, %v; want %+v", want, got, err, d)
	}
}

// srqppSide is the Go side of the exchange.
type srqppSide struct {
	dev     rdmatest.Dev
	ctx     *verbs.Context
	cq      *verbs.CQ
	srq     *verbs.SRQ
	qps     []*verbs.QP
	mr      *verbs.MR
	local   []srqppDest
	pending []int // the work requests each queue pair waits for
	routs   int   // receives posted to the SRQ and not completed yet
}

// newSRQPPSide sets up the Go side as pp_init_ctx does, with the SRQ's
// receives posted and the queue pairs in INIT.
func newSRQPPSide(t *testing.T) *srqppSide {
	t.Helper()
	dev, ctx := openDevice(t)
	s := &srqppSide{dev: dev, ctx: ctx}
	pd := allocPD(t, ctx)
	s.mr = allocMR(t, pd, srqppSize)
	cq, err := ctx.CreateCQ(srqppDepth+srqppQPs, nil)
	if err != nil {
		t.Fatalf("CreateCQ() error: %v", err)
	}
	t.Cleanup(func() { cq.Close() })
	s.cq = cq
	s.srq, _ = createSRQ(t, pd, srqppDepth)
	port := uint8(dev.Port)
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
	for range srqppQPs {
		qp, err := pd.CreateQP(&verbs.QPInitAttr{SendCQ: cq, RecvCQ: cq, SRQ: s.srq, Type: verbs.QPTypeRC, Cap: verbs.QPCap{MaxSendWR: 1, MaxSendSGE: 1}})
		if err != nil {
			t.Fatalf("CreateQP() with an SRQ error: %v", err)
		}
		t.Cleanup(func() { qp.Close() })
		err = qp.Modify(&verbs.QPAttr{State: verbs.QPStateInit, Port: port}, verbs.QPAttrState|verbs.QPAttrPkeyIndex|verbs.QPAttrPort|verbs.QPAttrAccessFlags)
		if err != nil {
			t.Fatalf("Modify(INIT) error: %v", err)
		}
		s.qps = append(s.qps, qp)
		s.local = append(s.local, srqppDest{lid: pa.LID, qpn: qp.Num(), psn: rand.Uint32() & 0xffffff, gid: gid})
	}
	s.postRecvs(t, srqppDepth)
	return s
}

// postRecvs posts n receives of the whole buffer to the SRQ.
func (s *srqppSide) postRecvs(t *testing.T, n int) {
	t.Helper()
	wrs := make([]verbs.RecvWR, n)
	for i := range wrs {
		wrs[i] = verbs.RecvWR{ID: srqppRecvWRID, SGE: []verbs.SGE{s.mr.SGE(s.mr.Bytes())}}
	}
	if err := s.srq.PostRecv(wrs...); err != nil {
		t.Fatalf("SRQ.PostRecv() of %d receives error: %v", n, err)
	}
	s.routs += n
}

// writeDests writes the srqppMaxQP addresses of the side, the unused ones
// zero.
func (s *srqppSide) writeDests(t *testing.T, conn net.Conn) {
	t.Helper()
	var b []byte
	for i := range srqppMaxQP {
		var d srqppDest
		if i < len(s.local) {
			d = s.local[i]
		}
		b = append(b, d.wire()...)
	}
	if _, err := conn.Write(b); err != nil {
		t.Fatalf("writing the local addresses: %v", err)
	}
}

// readDests reads the peer's srqppMaxQP addresses and returns those of the
// queue pairs in use.
func (s *srqppSide) readDests(t *testing.T, conn net.Conn) []srqppDest {
	t.Helper()
	b := make([]byte, srqppMaxQP*srqppMsgSize)
	if _, err := io.ReadFull(conn, b); err != nil {
		t.Fatalf("reading the remote addresses: %v", err)
	}
	dests := make([]srqppDest, srqppQPs)
	for i := range dests {
		d, err := parseSRQPPDest(b[i*srqppMsgSize : (i+1)*srqppMsgSize])
		if err != nil {
			t.Fatalf("remote address %d: %v", i, err)
		}
		dests[i] = d
	}
	return dests
}

// connect drives every queue pair to RTS towards its remote counterpart, as
// pp_connect_ctx does with a path MTU of 1024 and service level 0.
func (s *srqppSide) connect(t *testing.T, remote []srqppDest) {
	t.Helper()
	port := uint8(s.dev.Port)
	for i, qp := range s.qps {
		d := remote[i]
		rtr := verbs.QPAttr{
			State:           verbs.QPStateRTR,
			PathMTU:         verbs.MTU1024,
			DestQPNum:       d.qpn,
			RQPSN:           d.psn,
			MaxDestRDAtomic: 1,
			MinRNRTimer:     12,
			AH:              verbs.AHAttr{DLID: d.lid, Port: port},
		}
		// As the C program does, the GID of the first remote address decides
		// whether the path is global.
		if !slices.Equal(remote[0].gid[8:], make([]byte, 8)) {
			rtr.AH.IsGlobal = true
			rtr.AH.GRH = verbs.GlobalRoute{DGID: remote[0].gid, SGIDIndex: uint8(s.dev.GIDIndex), HopLimit: 1}
		}
		err := qp.Modify(&rtr, verbs.QPAttrState|verbs.QPAttrAV|verbs.QPAttrPathMTU|verbs.QPAttrDestQPN|verbs.QPAttrRQPSN|verbs.QPAttrMaxDestRDAtomic|verbs.QPAttrMinRNRTimer)
		if err != nil {
			t.Fatalf("Modify(queue pair %d to RTR) error: %v", i, err)
		}
		rts := verbs.QPAttr{State: verbs.QPStateRTS, Timeout: 14, RetryCnt: 7, RNRRetry: 7, SQPSN: s.local[i].psn, MaxRDAtomic: 1}
		err = qp.Modify(&rts, verbs.QPAttrState|verbs.QPAttrTimeout|verbs.QPAttrRetryCnt|verbs.QPAttrRNRRetry|verbs.QPAttrSQPSN|verbs.QPAttrMaxQPRDAtomic)
		if err != nil {
			t.Fatalf("Modify(queue pair %d to RTS) error: %v", i, err)
		}
	}
}

// postSend sends the buffer on queue pair i.
func (s *srqppSide) postSend(t *testing.T, i int) {
	t.Helper()
	err := s.qps[i].PostSend(verbs.SendWR{ID: srqppSendWRID, Op: verbs.WRSend, Flags: verbs.SendSignaled, SGE: []verbs.SGE{s.mr.SGE(s.mr.Bytes())}})
	if err != nil {
		t.Fatalf("PostSend() on queue pair %d error: %v", i, err)
	}
}

// pingpong runs the loop of the C program's main and returns how many
// messages the side received on each queue pair.
func (s *srqppSide) pingpong(t *testing.T, client bool) []int {
	t.Helper()
	s.pending = make([]int, len(s.qps))
	for i := range s.qps {
		s.pending[i] = srqppRecvWRID
		if client {
			s.postSend(t, i)
			s.pending[i] |= srqppSendWRID
		}
	}
	received := make([]int, len(s.qps))
	wcs := make([]verbs.WC, srqppDepth+srqppQPs)
	rcnt, scnt := 0, 0
	deadline := time.Now().Add(srqppTimeout)
	for rcnt < srqppIters || scnt < srqppIters {
		if time.Now().After(deadline) {
			t.Fatalf("after %v: %d of %d messages received, %d of %d sent", srqppTimeout, rcnt, srqppIters, scnt, srqppIters)
		}
		n, err := s.cq.Poll(wcs)
		if err != nil {
			t.Fatalf("Poll() error: %v", err)
		}
		if n == 0 {
			time.Sleep(50 * time.Microsecond)
		}
		for _, wc := range wcs[:n] {
			if err := wc.Err(); err != nil {
				t.Fatalf("completion of request %d failed: %v", wc.ID, err)
			}
			i := slices.IndexFunc(s.qps, func(qp *verbs.QP) bool { return qp.Num() == wc.QPNum })
			if i < 0 {
				t.Fatalf("completion %+v of an unknown queue pair", wc)
			}
			switch wc.ID {
			case srqppSendWRID:
				scnt++
			case srqppRecvWRID:
				if s.routs--; s.routs <= len(s.qps) {
					s.postRecvs(t, srqppDepth-s.routs)
				}
				rcnt++
				received[i]++
			default:
				t.Fatalf("completion %+v of an unknown request", wc)
			}
			s.pending[i] &^= int(wc.ID)
			if scnt < srqppIters && s.pending[i] == 0 {
				s.postSend(t, i)
				s.pending[i] = srqppRecvWRID | srqppSendWRID
			}
		}
	}
	return received
}

// srqPingpong finds ibv_srq_pingpong on PATH, skipping the test without it,
// or failing it under RDMA_TEST_REQUIRE=1.
func srqPingpong(t *testing.T) string {
	t.Helper()
	path, err := exec.LookPath("ibv_srq_pingpong")
	if err != nil {
		msg := "ibv_srq_pingpong (rdma-core's libibverbs examples) not found: " + err.Error()
		if os.Getenv("RDMA_TEST_REQUIRE") == "1" {
			t.Fatal(msg + " (RDMA_TEST_REQUIRE=1)")
		}
		t.Skip(msg)
	}
	return path
}

// startSRQPingpong runs ibv_srq_pingpong for the side's device with tcpPort
// and, for a client, the server's address, and returns a function that waits
// for it and checks that it exited 0 after its statistics.
func startSRQPingpong(t *testing.T, s *srqppSide, tcpPort int, server netip.Addr) (wait func()) {
	t.Helper()
	path := srqPingpong(t)
	args := []string{
		"-d", s.dev.Name, "-i", strconv.Itoa(s.dev.Port), "-g", strconv.Itoa(s.dev.GIDIndex),
		"-p", strconv.Itoa(tcpPort), "-q", strconv.Itoa(srqppQPs), "-r", strconv.Itoa(srqppDepth),
		"-s", strconv.Itoa(srqppSize), "-n", strconv.Itoa(srqppIters), "-m", "1024",
	}
	if server.IsValid() {
		args = append(args, server.String())
	}
	ctx, cancel := context.WithTimeout(context.Background(), srqppTimeout)
	cmd := exec.CommandContext(ctx, path, args...)
	var stdout, stderr bytes.Buffer
	cmd.Stdout, cmd.Stderr = &stdout, &stderr
	if err := cmd.Start(); err != nil {
		cancel()
		t.Fatalf("start %s: %v", path, err)
	}
	t.Cleanup(func() {
		cancel()
		cmd.Wait()
	})
	return func() {
		t.Helper()
		err := cmd.Wait()
		cancel()
		t.Logf("ibv_srq_pingpong %s exited with %v\nstdout:\n%sstderr:\n%s", strings.Join(args, " "), err, stdout.String(), stderr.String())
		if err != nil {
			t.Errorf("ibv_srq_pingpong %s: %v", strings.Join(args, " "), err)
		}
		if !strings.Contains(stdout.String(), " iters in ") {
			t.Errorf("ibv_srq_pingpong %s printed no statistics", strings.Join(args, " "))
		}
	}
}

// checkReceived checks that every queue pair received messages through the
// shared receive queue, srqppIters in all.
func checkReceived(t *testing.T, received []int) {
	t.Helper()
	total := 0
	for i, n := range received {
		if n == 0 {
			t.Errorf("queue pair %d received nothing through the SRQ: %v", i, received)
		}
		total += n
	}
	if total < srqppIters {
		t.Errorf("received %d messages through the SRQ, want at least %d: %v", total, srqppIters, received)
	}
	t.Logf("messages received per queue pair through the SRQ: %v", received)
}

// Go and rdma-core's ibv_srq_pingpong exchange messages on RC queue pairs
// that receive through an SRQ on either side, with Go as the server and as
// the client.
func TestSRQPingpongInterop(t *testing.T) {
	t.Run("ibv_srq_pingpong client", func(t *testing.T) {
		s := newSRQPPSide(t)
		ln, err := net.Listen("tcp", net.JoinHostPort(s.dev.IP.String(), "0"))
		if err != nil {
			t.Fatalf("Listen() error: %v", err)
		}
		defer ln.Close()
		wait := startSRQPingpong(t, s, ln.Addr().(*net.TCPAddr).Port, s.dev.IP)
		ln.(*net.TCPListener).SetDeadline(time.Now().Add(srqppTimeout))
		conn, err := ln.Accept()
		if err != nil {
			t.Fatalf("Accept() error: %v", err)
		}
		conn.SetDeadline(time.Now().Add(srqppTimeout))
		remote := s.readDests(t, conn)
		s.connect(t, remote)
		s.writeDests(t, conn)
		done := make([]byte, len("done")+1)
		if _, err := io.ReadFull(conn, done); err != nil || string(done) != "done\x00" {
			t.Fatalf("reading done: %q, %v", done, err)
		}
		conn.Close()
		checkReceived(t, s.pingpong(t, false))
		wait()
	})
	t.Run("ibv_srq_pingpong server", func(t *testing.T) {
		s := newSRQPPSide(t)
		ln, err := net.Listen("tcp", ":0")
		if err != nil {
			t.Fatalf("Listen() error: %v", err)
		}
		port := ln.Addr().(*net.TCPAddr).Port
		ln.Close()
		wait := startSRQPingpong(t, s, port, netip.Addr{})
		var conn net.Conn
		for deadline := time.Now().Add(10 * time.Second); ; time.Sleep(50 * time.Millisecond) {
			if conn, err = net.Dial("tcp", net.JoinHostPort(s.dev.IP.String(), strconv.Itoa(port))); err == nil || time.Now().After(deadline) {
				break
			}
		}
		if err != nil {
			t.Fatalf("Dial() to ibv_srq_pingpong error: %v", err)
		}
		conn.SetDeadline(time.Now().Add(srqppTimeout))
		s.writeDests(t, conn)
		remote := s.readDests(t, conn)
		if _, err := conn.Write([]byte("done\x00")); err != nil {
			t.Fatalf("writing done: %v", err)
		}
		conn.Close()
		s.connect(t, remote)
		checkReceived(t, s.pingpong(t, true))
		wait()
	})
}
