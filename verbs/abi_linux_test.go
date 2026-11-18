//go:build linux && !android && (amd64 || arm64 || loong64 || ppc64le || riscv64 || s390x)

package verbs

import (
	"bytes"
	"encoding/binary"
	"errors"
	"os"
	"syscall"
	"testing"
	"unsafe"

	"github.com/wzshiming/rdma/internal/rdmaabi"
)

// Immediate data travels in network byte order: 0x01020304 is the bytes
// 01 02 03 04 of the work request, and those bytes of a completion read back
// as 0x01020304.
func TestSendWRImmIsNetworkOrder(t *testing.T) {
	var cw rdmaabi.IbvSendWr
	sendWRToC(&cw, &SendWR{ID: 7, Op: WRSendImm, Imm: 0x01020304})
	if cw.Imm_data != [4]byte{1, 2, 3, 4} {
		t.Errorf("imm_data = % x, want 01 02 03 04", cw.Imm_data)
	}
	if cw.Wr_id != 7 || cw.Opcode != uint32(WRSendImm) {
		t.Errorf("wr_id, opcode = %d, %d; want 7, %d", cw.Wr_id, cw.Opcode, WRSendImm)
	}
}

// requestBytes returns the 128 bytes of struct ibv_send_wr that cw is.
func requestBytes(cw *rdmaabi.IbvSendWr) []byte {
	return unsafe.Slice((*byte)(unsafe.Pointer(cw)), unsafe.Sizeof(*cw))
}

// The request of a send on a connected queue pair, as the package built it
// before datagrams: its fields and the rdma member of the wr union at their
// C offsets, the chain's fields and the rest zero.
func TestRCSendRequestBytes(t *testing.T) {
	var cw rdmaabi.IbvSendWr
	sendWRToC(&cw, &SendWR{ID: 0x0102030405060708, Op: WRWriteImm, Flags: SendSignaled | SendSolicited, Imm: 0xa1b2c3d4, RemoteAddr: 0x1112131415161718, RKey: 0x21222324})
	want := make([]byte, 128)
	binary.NativeEndian.PutUint64(want[0:], 0x0102030405060708)  // wr_id
	binary.NativeEndian.PutUint32(want[28:], 1)                  // opcode IBV_WR_RDMA_WRITE_WITH_IMM
	binary.NativeEndian.PutUint32(want[32:], 2|4)                // send_flags IBV_SEND_SIGNALED|IBV_SEND_SOLICITED
	copy(want[36:], []byte{0xa1, 0xb2, 0xc3, 0xd4})              // imm_data
	binary.NativeEndian.PutUint64(want[40:], 0x1112131415161718) // wr.rdma.remote_addr
	binary.NativeEndian.PutUint32(want[48:], 0x21222324)         // wr.rdma.rkey
	if got := requestBytes(&cw); !bytes.Equal(got, want) {
		t.Errorf("request =\n% x\nwant\n% x", got, want)
	}
}

func TestWCImmIsNetworkOrder(t *testing.T) {
	wc := wcFromC(&rdmaabi.IbvWc{Wr_id: 3, Imm_data: [4]byte{1, 2, 3, 4}, Wc_flags: uint32(WCWithImm), Opcode: uint32(WCOpRecv)})
	if !wc.HasImm() || wc.Imm != 0x01020304 {
		t.Errorf("WC = %+v, want imm 0x01020304", wc)
	}
	if wc.ID != 3 || wc.Opcode != WCOpRecv {
		t.Errorf("WC = %+v, want id 3, RECV", wc)
	}
}

func TestGUIDFromBigEndianBytes(t *testing.T) {
	b := [8]byte{0x02, 0x50, 0xb6, 0xff, 0xfe, 0x0a, 0x4d, 0x8c}
	const want = 0x0250b6fffe0a4d8c
	if got := guid(b); got != want {
		t.Errorf("guid(% x) = %#x, want %#x", b, got, want)
	}
	// ibv_get_device_guid hands the same bytes back loaded into a register.
	if got := be64(binary.NativeEndian.Uint64(b[:])); got != want {
		t.Errorf("be64(register of % x) = %#x, want %#x", b, got, want)
	}
}

func TestGIDEntryKeepsTheBytes(t *testing.T) {
	gid := rdmaabi.IbvGid{0: 0xfe, 1: 0x80, 14: 0xab, 15: 0x01}
	got := gidEntryFromC(&rdmaabi.IbvGidEntry{Gid: gid, Gid_index: 3, Port_num: 1, Gid_type: 2})
	want := GIDEntry{Index: 3, Type: GIDTypeRoCEv2, GID: GID{0: 0xfe, 1: 0x80, 14: 0xab, 15: 0x01}}
	if got != want {
		t.Errorf("gidEntryFromC() = %+v, want %+v", got, want)
	}
	if got.GID.String() != "fe80::ab01" {
		t.Errorf("GID = %v, want fe80::ab01", got.GID)
	}
}

// ahToC puts each field where struct ibv_ah_attr has it, and ahFromC reads
// it back from there.
func TestAHAttrRoundTripsThroughC(t *testing.T) {
	a := AHAttr{
		GRH:         GlobalRoute{DGID: GID{0: 0xfe, 1: 0x80, 15: 0x01}, FlowLabel: 0x12345, SGIDIndex: 8, HopLimit: 64, TrafficClass: 2},
		DLID:        5,
		SL:          3,
		SrcPathBits: 4,
		StaticRate:  6,
		IsGlobal:    true,
		Port:        7,
	}
	want := rdmaabi.IbvAhAttr{
		Grh:           rdmaabi.IbvGlobalRoute{Dgid: rdmaabi.IbvGid{0: 0xfe, 1: 0x80, 15: 0x01}, Flow_label: 0x12345, Sgid_index: 8, Hop_limit: 64, Traffic_class: 2},
		Dlid:          5,
		Sl:            3,
		Src_path_bits: 4,
		Static_rate:   6,
		Is_global:     1,
		Port_num:      7,
	}
	ca := ahToC(a)
	if ca != want {
		t.Errorf("ahToC(a) = %+v, want %+v", ca, want)
	}
	if got := ahFromC(&ca); got != a {
		t.Errorf("ahFromC(ahToC(a)) = %+v, want %+v", got, a)
	}
}

// cMemory returns size zeroed bytes outside the Go heap, where the library's
// own structures live, unmapped when the test ends.
func cMemory(t *testing.T, size int) unsafe.Pointer {
	t.Helper()
	b, err := syscall.Mmap(-1, 0, size, syscall.PROT_READ|syscall.PROT_WRITE, syscall.MAP_ANON|syscall.MAP_PRIVATE)
	if err != nil {
		t.Fatalf("mmap: %v", err)
	}
	t.Cleanup(func() { syscall.Munmap(b) })
	return unsafe.Pointer(unsafe.SliceData(b))
}

// The transport type is the enum ibv_transport_type libibverbs stores in
// struct ibv_device next to the node type it derives it from; a context
// reaches that struct through its device member.
func TestTransportTypeReadsStructIbvDevice(t *testing.T) {
	cdev := (*rdmaabi.IbvDevice)(cMemory(t, os.Getpagesize()))
	cctx := (*rdmaabi.IbvContext)(cMemory(t, os.Getpagesize()))
	cctx.Device = uint64(uintptr(unsafe.Pointer(cdev)))
	dev := &Device{dev: uintptr(unsafe.Pointer(cdev))}
	ctx := ContextFromPointer(unsafe.Pointer(cctx))
	tests := []struct {
		node, transport int32 // enum ibv_node_type, enum ibv_transport_type
		want            TransportType
	}{
		{1, 0, TransportIB},          // IBV_NODE_CA, IBV_TRANSPORT_IB: InfiniBand and RoCE
		{4, 1, TransportIWARP},       // IBV_NODE_RNIC, IBV_TRANSPORT_IWARP
		{5, 2, TransportUSNIC},       // IBV_NODE_USNIC, IBV_TRANSPORT_USNIC
		{6, 3, TransportUSNICUDP},    // IBV_NODE_USNIC_UDP, IBV_TRANSPORT_USNIC_UDP
		{7, 4, TransportUnspecified}, // IBV_NODE_UNSPECIFIED, IBV_TRANSPORT_UNSPECIFIED
		{-1, -1, TransportUnknown},   // IBV_NODE_UNKNOWN, IBV_TRANSPORT_UNKNOWN
	}
	for _, tt := range tests {
		cdev.Node_type, cdev.Transport_type = tt.node, tt.transport
		if got := dev.TransportType(); got != tt.want {
			t.Errorf("Device.TransportType() with transport_type %d = %v, want %v", tt.transport, got, tt.want)
		}
		if got := ctx.TransportType(); got != tt.want {
			t.Errorf("Context.TransportType() with transport_type %d = %v, want %v", tt.transport, got, tt.want)
		}
	}
}

// fakeQP returns a queue pair of type typ in C memory, on a context whose ops
// table is empty: requests can be built for it, never posted.
func fakeQP(t *testing.T, typ QPType) *QP {
	t.Helper()
	cctx := (*rdmaabi.IbvContext)(cMemory(t, os.Getpagesize()))
	cqp := (*rdmaabi.IbvQp)(cMemory(t, os.Getpagesize()))
	cqp.Context = uint64(uintptr(unsafe.Pointer(cctx)))
	cqp.Qp_type = uint32(typ)
	return QPFromPointer(unsafe.Pointer(cqp))
}

// fakeCQ returns a completion queue in C memory on a context whose ops table
// is empty: it can be named, never polled.
func fakeCQ(t *testing.T) *CQ {
	t.Helper()
	cctx := (*rdmaabi.IbvContext)(cMemory(t, os.Getpagesize()))
	ccq := (*rdmaabi.IbvCq)(cMemory(t, os.Getpagesize()))
	ccq.Context = uint64(uintptr(unsafe.Pointer(cctx)))
	return CQFromPointer(unsafe.Pointer(ccq))
}

// The struct ibv_qp_init_attr of ibv_create_qp: the CQs, the capacities, the
// type and sq_sig_all at their C offsets, the rest zero.
func TestQPInitAttrBytes(t *testing.T) {
	sendCQ, recvCQ := fakeCQ(t), fakeCQ(t)
	ia, err := qpInitAttrToC(&QPInitAttr{
		SendCQ:    sendCQ,
		RecvCQ:    recvCQ,
		Type:      QPTypeRC,
		Cap:       QPCap{MaxSendWR: 1, MaxRecvWR: 2, MaxSendSGE: 3, MaxRecvSGE: 4, MaxInlineData: 5},
		SignalAll: true,
	})
	if err != nil {
		t.Fatalf("qpInitAttrToC() error: %v", err)
	}
	want := make([]byte, 64)
	binary.NativeEndian.PutUint64(want[8:], uint64(uintptr(sendCQ.Pointer())))  // send_cq
	binary.NativeEndian.PutUint64(want[16:], uint64(uintptr(recvCQ.Pointer()))) // recv_cq
	for i, v := range []uint32{1, 2, 3, 4, 5} {
		binary.NativeEndian.PutUint32(want[32+4*i:], v) // cap
	}
	binary.NativeEndian.PutUint32(want[52:], 2) // qp_type IBV_QPT_RC
	binary.NativeEndian.PutUint32(want[56:], 1) // sq_sig_all
	if got := unsafe.Slice((*byte)(unsafe.Pointer(&ia)), unsafe.Sizeof(ia)); !bytes.Equal(got, want) {
		t.Errorf("init attr =\n% x\nwant\n% x", got, want)
	}
}

// A send of a UD queue pair carries its destination where other requests
// carry their RDMA address: the address handle, the remote queue pair number
// and qkey in the ud member of the wr union.
func TestDatagramSendRequestBytes(t *testing.T) {
	ah := AHFromPointer(cMemory(t, 64))
	w := SendWR{ID: 0x0102030405060708, Op: WRSendImm, Flags: SendSignaled, Imm: 0xa1b2c3d4, RemoteAddr: 0x1112131415161718, RKey: 0x21222324, AH: ah, RemoteQPN: 0x00abcdef, RemoteQKey: 0x01234567}
	var cw rdmaabi.IbvSendWr
	if err := fakeQP(t, QPTypeUD).requestToC(&cw, &w); err != nil {
		t.Fatalf("requestToC() error: %v", err)
	}
	want := make([]byte, 128)
	binary.NativeEndian.PutUint64(want[0:], 0x0102030405060708)             // wr_id
	binary.NativeEndian.PutUint32(want[28:], 3)                             // opcode IBV_WR_SEND_WITH_IMM
	binary.NativeEndian.PutUint32(want[32:], 2)                             // send_flags IBV_SEND_SIGNALED
	copy(want[36:], []byte{0xa1, 0xb2, 0xc3, 0xd4})                         // imm_data
	binary.NativeEndian.PutUint64(want[40:], uint64(uintptr(ah.Pointer()))) // wr.ud.ah
	binary.NativeEndian.PutUint32(want[48:], 0x00abcdef)                    // wr.ud.remote_qpn
	binary.NativeEndian.PutUint32(want[52:], 0x01234567)                    // wr.ud.remote_qkey
	if got := requestBytes(&cw); !bytes.Equal(got, want) {
		t.Errorf("request =\n% x\nwant\n% x", got, want)
	}
}

// The datagram fields of a request change nothing on a connected queue pair.
func TestRCSendRequestIgnoresDatagramFields(t *testing.T) {
	w := SendWR{ID: 0x0102030405060708, Op: WRWriteImm, Flags: SendSignaled | SendSolicited, Imm: 0xa1b2c3d4, RemoteAddr: 0x1112131415161718, RKey: 0x21222324}
	var want rdmaabi.IbvSendWr
	sendWRToC(&want, &w)
	w.AH, w.RemoteQPN, w.RemoteQKey = AHFromPointer(cMemory(t, 64)), 0x00abcdef, 0x01234567
	var got rdmaabi.IbvSendWr
	if err := fakeQP(t, QPTypeRC).requestToC(&got, &w); err != nil {
		t.Fatalf("requestToC() error: %v", err)
	}
	if !bytes.Equal(requestBytes(&got), requestBytes(&want)) {
		t.Errorf("request =\n% x\nwant\n% x", requestBytes(&got), requestBytes(&want))
	}
}

// A UD queue pair refuses a datagram without an address handle before the
// provider follows the null pointer, and posts none of the chain: the post
// function of a fake queue pair is at address zero.
func TestDatagramWithoutAHIsInvalid(t *testing.T) {
	ah := AHFromPointer(cMemory(t, 64))
	err := fakeQP(t, QPTypeUD).PostSend(SendWR{ID: 1, Op: WRSend, AH: ah}, SendWR{ID: 2, Op: WRSend})
	var opErr *OpError
	if !errors.As(err, &opErr) || opErr.Op != "ibv_post_send" || !errors.Is(err, syscall.EINVAL) {
		t.Errorf("PostSend() of a datagram without an address handle error = %v, want an *OpError of ibv_post_send matching EINVAL", err)
	}
}

// A closed address handle refuses to be sent to, as a closed CQ refuses to
// be polled.
func TestDatagramToAClosedAHIsClosed(t *testing.T) {
	ah := AHFromPointer(cMemory(t, 64))
	if err := ah.Close(); err != nil {
		t.Fatalf("Close() error: %v", err)
	}
	err := fakeQP(t, QPTypeUD).PostSend(SendWR{ID: 1, Op: WRSend, AH: ah})
	var opErr *OpError
	if !errors.As(err, &opErr) || opErr.Op != "ibv_post_send" || !errors.Is(err, os.ErrClosed) {
		t.Errorf("PostSend() to a closed address handle error = %v, want an *OpError of ibv_post_send matching os.ErrClosed", err)
	}
}

// fakeSRQ returns a shared receive queue in C memory on a context whose ops
// table is empty: requests can be built for it, never posted.
func fakeSRQ(t *testing.T) *SRQ {
	t.Helper()
	cctx := (*rdmaabi.IbvContext)(cMemory(t, os.Getpagesize()))
	csrq := (*rdmaabi.IbvSrq)(cMemory(t, os.Getpagesize()))
	csrq.Context = uint64(uintptr(unsafe.Pointer(cctx)))
	return SRQFromPointer(unsafe.Pointer(csrq))
}

// srqAttrToC puts each field where struct ibv_srq_attr has it, and
// srqAttrFromC reads it back from there.
func TestSRQAttrRoundTripsThroughC(t *testing.T) {
	a := SRQAttr{MaxWR: 0x01020304, MaxSGE: 5, Limit: 6}
	want := rdmaabi.IbvSrqAttr{Max_wr: 0x01020304, Max_sge: 5, Srq_limit: 6}
	ca := srqAttrToC(a)
	if ca != want {
		t.Errorf("srqAttrToC(a) = %+v, want %+v", ca, want)
	}
	if got := srqAttrFromC(ca); got != a {
		t.Errorf("srqAttrFromC(srqAttrToC(a)) = %+v, want %+v", got, a)
	}
}

// A shared receive queue posts through post_srq_recv, the slot of its
// context's ops table that ibv_post_srq_recv calls, not through the
// post_recv of queue pairs.
func TestSRQPostsThroughPostSRQRecv(t *testing.T) {
	cctx := (*rdmaabi.IbvContext)(cMemory(t, os.Getpagesize()))
	cctx.Ops.Post_srq_recv, cctx.Ops.Post_recv = 0x1000, 0x2000
	csrq := (*rdmaabi.IbvSrq)(cMemory(t, os.Getpagesize()))
	csrq.Context = uint64(uintptr(unsafe.Pointer(cctx)))
	if got := SRQFromPointer(unsafe.Pointer(csrq)).postRecv.Addr(); got != 0x1000 {
		t.Errorf("SRQ posts through %#x, want post_srq_recv at %#x", got, 0x1000)
	}
}

// The receive requests of a shared receive queue, built in its own buffer:
// struct ibv_recv_wr linked by next, each pointing at its SGEs.
func TestSRQReceiveRequestBytes(t *testing.T) {
	srq := fakeSRQ(t)
	srq.recv.mu.Lock()
	defer srq.recv.mu.Unlock()
	first := recvChain(&srq.recv, []RecvWR{
		{ID: 0x0102030405060708, SGE: []SGE{{Addr: 0x1112131415161718, Length: 0x21222324, LKey: 0x31323334}, {Addr: 0x4142434445464748, Length: 0x51525354, LKey: 0x61626364}}},
		{ID: 9, SGE: []SGE{{Addr: 0x7172737475767778, Length: 10, LKey: 11}}},
	})
	wrs, sges := srq.recv.wrs[:2], srq.recv.sges[:3]
	if first != &wrs[0] {
		t.Fatalf("the chain starts at %p, want the buffer's first request %p", first, &wrs[0])
	}
	want := make([]byte, 64)
	binary.NativeEndian.PutUint64(want[0:], 0x0102030405060708)                         // [0].wr_id
	binary.NativeEndian.PutUint64(want[8:], uint64(uintptr(unsafe.Pointer(&wrs[1]))))   // [0].next
	binary.NativeEndian.PutUint64(want[16:], uint64(uintptr(unsafe.Pointer(&sges[0])))) // [0].sg_list
	binary.NativeEndian.PutUint32(want[24:], 2)                                         // [0].num_sge
	binary.NativeEndian.PutUint64(want[32:], 9)                                         // [1].wr_id
	binary.NativeEndian.PutUint64(want[48:], uint64(uintptr(unsafe.Pointer(&sges[2])))) // [1].sg_list
	binary.NativeEndian.PutUint32(want[56:], 1)                                         // [1].num_sge
	if got := unsafe.Slice((*byte)(unsafe.Pointer(first)), 64); !bytes.Equal(got, want) {
		t.Errorf("requests =\n% x\nwant\n% x", got, want)
	}
	wantSGEs := []rdmaabi.IbvSge{
		{Addr: 0x1112131415161718, Length: 0x21222324, Lkey: 0x31323334},
		{Addr: 0x4142434445464748, Length: 0x51525354, Lkey: 0x61626364},
		{Addr: 0x7172737475767778, Length: 10, Lkey: 11},
	}
	for i := range wantSGEs {
		if sges[i] != wantSGEs[i] {
			t.Errorf("SGE %d = %+v, want %+v", i, sges[i], wantSGEs[i])
		}
	}
}

// A closed shared receive queue refuses work, as a closed CQ refuses to be
// polled; a second Close does nothing.
func TestClosedSRQRefusesWork(t *testing.T) {
	srq := fakeSRQ(t)
	if err := srq.Close(); err != nil {
		t.Fatalf("Close() error: %v", err)
	}
	if err := srq.Close(); err != nil {
		t.Errorf("second Close() error: %v", err)
	}
	for op, call := range map[string]func() error{
		"ibv_post_srq_recv": func() error { return srq.PostRecv(RecvWR{ID: 1}) },
		"ibv_modify_srq":    func() error { return srq.Modify(&SRQAttr{Limit: 1}, SRQAttrLimit) },
		"ibv_query_srq":     func() error { _, err := srq.Query(); return err },
		"ibv_get_srq_num":   func() error { _, err := srq.Num(); return err },
	} {
		err := call()
		var opErr *OpError
		if !errors.As(err, &opErr) || opErr.Op != op || !errors.Is(err, os.ErrClosed) {
			t.Errorf("%s on a closed SRQ: error = %v, want an *OpError of %s matching os.ErrClosed", op, err, op)
		}
	}
}

// A queue pair that receives through a shared receive queue names it in its
// struct ibv_qp_init_attr.
func TestQPInitAttrNamesTheSRQ(t *testing.T) {
	srq := fakeSRQ(t)
	ia, err := qpInitAttrToC(&QPInitAttr{SendCQ: fakeCQ(t), Type: QPTypeRC, SRQ: srq})
	if err != nil {
		t.Fatalf("qpInitAttrToC() error: %v", err)
	}
	if ia.Srq != uint64(uintptr(srq.Pointer())) {
		t.Errorf("srq = %#x, want %p", ia.Srq, srq.Pointer())
	}
}

// A queue pair is not created on a closed shared receive queue, which the
// provider would take for none and give the queue pair a receive queue of
// its own.
func TestCreateQPOnAClosedSRQIsClosed(t *testing.T) {
	srq := fakeSRQ(t)
	if err := srq.Close(); err != nil {
		t.Fatalf("Close() error: %v", err)
	}
	_, err := PDFromPointer(cMemory(t, 64)).CreateQP(&QPInitAttr{SendCQ: fakeCQ(t), Type: QPTypeRC, SRQ: srq})
	var opErr *OpError
	if !errors.As(err, &opErr) || opErr.Op != "ibv_create_qp" || !errors.Is(err, os.ErrClosed) {
		t.Errorf("CreateQP() on a closed SRQ: error = %v, want an *OpError of ibv_create_qp matching os.ErrClosed", err)
	}
}

// The asynchronous events of a shared receive queue name it.
func TestSRQEventNamesItsSRQ(t *testing.T) {
	srq := fakeSRQ(t)
	for _, typ := range []EventType{EventSRQErr, EventSRQLimitReached} {
		cev := rdmaabi.IbvAsyncEvent{Event_type: uint32(typ)}
		binary.NativeEndian.PutUint64(cev.Element[:], uint64(uintptr(srq.Pointer())))
		ev := asyncEventFromC(&cev)
		if ev.Type != typ || ev.SRQ == nil || ev.SRQ.Pointer() != srq.Pointer() || ev.QP != nil || ev.CQ != nil {
			t.Errorf("asyncEventFromC(%v on %p) = %+v, want the event on that SRQ only", typ, srq.Pointer(), ev)
		}
	}
}

// A send of an XRC send queue pair names the SRQ it goes to in the xrc
// member of the qp_type union, after the RDMA address it may carry.
func TestXRCSendRequestBytes(t *testing.T) {
	w := SendWR{ID: 0x0102030405060708, Op: WRWriteImm, Flags: SendSignaled, Imm: 0xa1b2c3d4, RemoteAddr: 0x1112131415161718, RKey: 0x21222324, RemoteSRQN: 0x00abcdef}
	var cw rdmaabi.IbvSendWr
	if err := fakeQP(t, QPTypeXRCSend).requestToC(&cw, &w); err != nil {
		t.Fatalf("requestToC() error: %v", err)
	}
	want := make([]byte, 128)
	binary.NativeEndian.PutUint64(want[0:], 0x0102030405060708)  // wr_id
	binary.NativeEndian.PutUint32(want[28:], 1)                  // opcode IBV_WR_RDMA_WRITE_WITH_IMM
	binary.NativeEndian.PutUint32(want[32:], 2)                  // send_flags IBV_SEND_SIGNALED
	copy(want[36:], []byte{0xa1, 0xb2, 0xc3, 0xd4})              // imm_data
	binary.NativeEndian.PutUint64(want[40:], 0x1112131415161718) // wr.rdma.remote_addr
	binary.NativeEndian.PutUint32(want[48:], 0x21222324)         // wr.rdma.rkey
	binary.NativeEndian.PutUint32(want[72:], 0x00abcdef)         // qp_type.xrc.remote_srqn
	if got := requestBytes(&cw); !bytes.Equal(got, want) {
		t.Errorf("request =\n% x\nwant\n% x", got, want)
	}
}

// The remote SRQ number of a request changes nothing on the queue pairs of
// other types.
func TestRemoteSRQNIsOnlyForXRC(t *testing.T) {
	ah := AHFromPointer(cMemory(t, 64))
	for _, typ := range []QPType{QPTypeRC, QPTypeUD} {
		w := SendWR{ID: 7, Op: WRSend, Flags: SendSignaled, RemoteAddr: 0x1112131415161718, RKey: 0x21222324, AH: ah, RemoteQPN: 5, RemoteQKey: 6}
		var want, got rdmaabi.IbvSendWr
		if err := fakeQP(t, typ).requestToC(&want, &w); err != nil {
			t.Fatalf("%v: requestToC() error: %v", typ, err)
		}
		w.RemoteSRQN = 0x00abcdef
		if err := fakeQP(t, typ).requestToC(&got, &w); err != nil {
			t.Fatalf("%v: requestToC() with a remote SRQ number error: %v", typ, err)
		}
		if !bytes.Equal(requestBytes(&got), requestBytes(&want)) {
			t.Errorf("%v: request with a remote SRQ number =\n% x\nwant\n% x", typ, requestBytes(&got), requestBytes(&want))
		}
	}
}

// The struct ibv_xrcd_init_attr of OpenXRCD: the descriptor and the open
// flags, with the comp_mask bits of both, which ibv_cmd_open_xrcd requires.
func TestXRCDInitAttr(t *testing.T) {
	got := xrcdInitAttrToC(7, syscall.O_CREAT|syscall.O_EXCL)
	want := rdmaabi.IbvXrcdInitAttr{Comp_mask: 1<<0 | 1<<1, Fd: 7, Oflags: syscall.O_CREAT | syscall.O_EXCL}
	if got != want {
		t.Errorf("xrcdInitAttrToC(7, O_CREAT|O_EXCL) = %+v, want %+v", got, want)
	}
}

// fakeXRCD returns an XRC domain in C memory.
func fakeXRCD(t *testing.T) *XRCD {
	t.Helper()
	return XRCDFromPointer(cMemory(t, 64))
}

// The struct ibv_srq_init_attr_ex of an XRC SRQ: its attributes, its type,
// PD, domain and CQ, and the comp_mask bits that select them.
func TestXRCSRQInitAttrBytes(t *testing.T) {
	pd, xrcd, cq := cMemory(t, 64), fakeXRCD(t), fakeCQ(t)
	ia, err := srqInitAttrExToC(&SRQInitAttr{Type: SRQTypeXRC, Attr: SRQAttr{MaxWR: 1, MaxSGE: 2, Limit: 3}, XRCD: xrcd, CQ: cq}, pd)
	if err != nil {
		t.Fatalf("srqInitAttrExToC() error: %v", err)
	}
	want := make([]byte, 64)
	binary.NativeEndian.PutUint32(want[8:], 1)                                // attr.max_wr
	binary.NativeEndian.PutUint32(want[12:], 2)                               // attr.max_sge
	binary.NativeEndian.PutUint32(want[16:], 3)                               // attr.srq_limit
	binary.NativeEndian.PutUint32(want[20:], 1|2|4|8)                         // comp_mask TYPE|PD|XRCD|CQ
	binary.NativeEndian.PutUint32(want[24:], 1)                               // srq_type IBV_SRQT_XRC
	binary.NativeEndian.PutUint64(want[32:], uint64(uintptr(pd)))             // pd
	binary.NativeEndian.PutUint64(want[40:], uint64(uintptr(xrcd.Pointer()))) // xrcd
	binary.NativeEndian.PutUint64(want[48:], uint64(uintptr(cq.Pointer())))   // cq
	if got := unsafe.Slice((*byte)(unsafe.Pointer(&ia)), unsafe.Sizeof(ia)); !bytes.Equal(got, want) {
		t.Errorf("init attr =\n% x\nwant\n% x", got, want)
	}
}

// An XRC SRQ needs its domain and its CQ, whose pointers the provider
// follows: without one the attributes are invalid, and a closed one is
// refused.
func TestXRCSRQWithoutItsDomainOrCQ(t *testing.T) {
	closedXRCD, closedCQ := fakeXRCD(t), fakeCQ(t)
	closedXRCD.Close()
	closedCQ.Close()
	tests := []struct {
		name string
		xrcd *XRCD
		cq   *CQ
		want error
	}{
		{"no domain", nil, fakeCQ(t), syscall.EINVAL},
		{"no CQ", fakeXRCD(t), nil, syscall.EINVAL},
		{"closed domain", closedXRCD, fakeCQ(t), os.ErrClosed},
		{"closed CQ", fakeXRCD(t), closedCQ, os.ErrClosed},
	}
	for _, tt := range tests {
		_, err := PDFromPointer(cMemory(t, 64)).CreateSRQ(&SRQInitAttr{Type: SRQTypeXRC, Attr: SRQAttr{MaxWR: 1, MaxSGE: 1}, XRCD: tt.xrcd, CQ: tt.cq})
		var opErr *OpError
		if !errors.As(err, &opErr) || opErr.Op != "ibv_create_srq_ex" || !errors.Is(err, tt.want) {
			t.Errorf("%s: CreateSRQ() error = %v, want an *OpError of ibv_create_srq_ex matching %v", tt.name, err, tt.want)
		}
	}
	_, err := PDFromPointer(cMemory(t, 64)).CreateSRQ(&SRQInitAttr{Type: SRQType(2), Attr: SRQAttr{MaxWR: 1, MaxSGE: 1}})
	var opErr *OpError
	if !errors.As(err, &opErr) || opErr.Op != "ibv_create_srq_ex" || !errors.Is(err, syscall.EINVAL) {
		t.Errorf("CreateSRQ() of type 2 error = %v, want an *OpError of ibv_create_srq_ex matching EINVAL", err)
	}
}

// The struct ibv_qp_init_attr_ex of a queue pair in an XRC domain: the
// members of struct ibv_qp_init_attr, then the comp_mask bits of the PD and
// the domain and their pointers.
func TestXRCQPInitAttrBytes(t *testing.T) {
	pd, xrcd := cMemory(t, 64), fakeXRCD(t)
	ia, err := qpInitAttrExToC(&QPInitAttr{Type: QPTypeXRCRecv, XRCD: xrcd}, pd)
	if err != nil {
		t.Fatalf("qpInitAttrExToC() error: %v", err)
	}
	want := make([]byte, 136)
	binary.NativeEndian.PutUint32(want[52:], 10)                              // qp_type IBV_QPT_XRC_RECV
	binary.NativeEndian.PutUint32(want[60:], 1|2)                             // comp_mask PD|XRCD
	binary.NativeEndian.PutUint64(want[64:], uint64(uintptr(pd)))             // pd
	binary.NativeEndian.PutUint64(want[72:], uint64(uintptr(xrcd.Pointer()))) // xrcd
	if got := unsafe.Slice((*byte)(unsafe.Pointer(&ia)), unsafe.Sizeof(ia)); !bytes.Equal(got, want) {
		t.Errorf("init attr =\n% x\nwant\n% x", got, want)
	}
}

// A queue pair is not created in a closed XRC domain, nor is a domain opened
// on a closed context.
func TestClosedXRCObjectsAreRefused(t *testing.T) {
	xrcd := fakeXRCD(t)
	xrcd.Close()
	_, err := PDFromPointer(cMemory(t, 64)).CreateQP(&QPInitAttr{Type: QPTypeXRCRecv, XRCD: xrcd})
	var opErr *OpError
	if !errors.As(err, &opErr) || opErr.Op != "ibv_create_qp_ex" || !errors.Is(err, os.ErrClosed) {
		t.Errorf("CreateQP() in a closed XRC domain: error = %v, want an *OpError of ibv_create_qp_ex matching os.ErrClosed", err)
	}
	ctx := ContextFromPointer(cMemory(t, os.Getpagesize()))
	ctx.Close()
	if _, err := ctx.OpenXRCD(-1, syscall.O_CREAT); !errors.As(err, &opErr) || opErr.Op != "ibv_open_xrcd" || !errors.Is(err, os.ErrClosed) {
		t.Errorf("OpenXRCD() on a closed context: error = %v, want an *OpError of ibv_open_xrcd matching os.ErrClosed", err)
	}
}
