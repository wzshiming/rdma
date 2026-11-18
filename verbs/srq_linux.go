//go:build linux && !android && (amd64 || arm64 || loong64 || ppc64le || riscv64 || s390x)

package verbs

import (
	"os"
	"sync"
	"syscall"
	"unsafe"

	"github.com/wzshiming/rdma/internal/dynlib"
	"github.com/wzshiming/rdma/internal/rdmaabi"
)

// srqs maps struct ibv_srq pointers to the SRQs this package created, so
// that asynchronous events can name their Go object.
var srqs sync.Map

// SRQ is a shared receive queue (struct ibv_srq): the receive queue of every
// queue pair created with it in QPInitAttr.SRQ, or, of type SRQTypeXRC, the
// destination of the sends of XRC send queue pairs that name its number.
type SRQ struct {
	srq      *rdmaabi.IbvSrq
	owned    bool
	postRecv dynlib.Func // post_srq_recv of the device context
	recv     postBuf[rdmaabi.IbvRecvWr]
}

// newSRQ wraps srq, taking the inline verb from its context's ops table.
func newSRQ(srq *rdmaabi.IbvSrq, owned bool) *SRQ {
	s := &SRQ{srq: srq, owned: owned}
	if srq != nil {
		s.postRecv = dynlib.FuncAt(uintptr(contextOps(srq.Context).Post_srq_recv))
	}
	return s
}

// CreateSRQ creates a shared receive queue and stores the capacities granted
// in attr.Attr: one of type SRQTypeBasic through ibv_create_srq, one of type
// SRQTypeXRC through the create_srq_ex of its domain's device, as
// ibv_create_srq_ex does, which reports syscall.EOPNOTSUPP on a device or
// library without it.
func (p *PD) CreateSRQ(attr *SRQInitAttr) (*SRQ, error) {
	switch attr.Type {
	case SRQTypeBasic:
		return p.createSRQ(attr)
	case SRQTypeXRC:
		return p.createXRCSRQ(attr)
	}
	return nil, &OpError{Op: "ibv_create_srq_ex", Err: syscall.EINVAL}
}

func (p *PD) createSRQ(attr *SRQInitAttr) (*SRQ, error) {
	f, err := load("ibv_create_srq")
	if err != nil {
		return nil, err
	}
	ia := rdmaabi.IbvSrqInitAttr{Attr: srqAttrToC(attr.Attr)}
	srq, errno := f.createSRQ.Ptr(uintptr(p.pd), uintptr(unsafe.Pointer(&ia)), 0, 0, 0, 0)
	if srq == 0 {
		return nil, opError("ibv_create_srq", errno)
	}
	attr.Attr = srqAttrFromC(ia.Attr)
	s := newSRQ(dynlib.Pointer[rdmaabi.IbvSrq](srq), true)
	srqs.Store(srq, s)
	return s, nil
}

func (p *PD) createXRCSRQ(attr *SRQInitAttr) (*SRQ, error) {
	ia, err := srqInitAttrExToC(attr, p.pd)
	if err != nil {
		return nil, &OpError{Op: "ibv_create_srq_ex", Err: err}
	}
	ctx := uintptr(attr.XRCD.xrcd.Context)
	create := verbsOp(ctx, unsafe.Offsetof(rdmaabi.VerbsContext{}.Create_srq_ex))
	if create.Addr() == 0 {
		return nil, &OpError{Op: "ibv_create_srq_ex", Err: syscall.EOPNOTSUPP}
	}
	srq, errno := create.Ptr(ctx, uintptr(unsafe.Pointer(&ia)), 0, 0, 0, 0)
	if srq == 0 {
		return nil, opError("ibv_create_srq_ex", errno)
	}
	attr.Attr = srqAttrFromC(ia.Attr)
	s := newSRQ(dynlib.Pointer[rdmaabi.IbvSrq](srq), true)
	srqs.Store(srq, s)
	return s, nil
}

// srqInitAttrExToC converts attr, of an XRC SRQ on the struct ibv_pd at pd,
// into the struct ibv_srq_init_attr_ex of create_srq_ex. The provider
// follows the pointers of the domain and the CQ: a missing one is invalid
// and a closed one refused with os.ErrClosed.
func srqInitAttrExToC(attr *SRQInitAttr, pd unsafe.Pointer) (rdmaabi.IbvSrqInitAttrEx, error) {
	var ia rdmaabi.IbvSrqInitAttrEx
	if attr.XRCD == nil || attr.CQ == nil {
		return ia, syscall.EINVAL
	}
	if attr.XRCD.xrcd == nil || attr.CQ.cq == nil {
		return ia, os.ErrClosed
	}
	ia.Attr = srqAttrToC(attr.Attr)
	// IBV_SRQ_INIT_ATTR_TYPE, _PD, _XRCD and _CQ
	ia.Comp_mask = 1<<0 | 1<<1 | 1<<2 | 1<<3
	ia.Srq_type = uint32(SRQTypeXRC)
	ia.Pd = uint64(uintptr(pd))
	ia.Xrcd = uint64(caddr(attr.XRCD.xrcd))
	ia.Cq = uint64(caddr(attr.CQ.cq))
	return ia, nil
}

// lookupSRQ returns the SRQ created for the struct ibv_srq at addr, or a
// non-owning wrapper.
func lookupSRQ(addr uintptr) *SRQ {
	if v, ok := srqs.Load(addr); ok {
		return v.(*SRQ)
	}
	return newSRQ(dynlib.Pointer[rdmaabi.IbvSrq](addr), false)
}

// SRQFromPointer wraps a struct ibv_srq pointer owned by someone else; Close
// does not destroy it.
func SRQFromPointer(p unsafe.Pointer) *SRQ { return newSRQ((*rdmaabi.IbvSrq)(p), false) }

// Pointer returns the underlying struct ibv_srq pointer.
func (s *SRQ) Pointer() unsafe.Pointer { return unsafe.Pointer(s.srq) }

// PostRecv posts receive work requests to the shared receive queue as one
// chain (ibv_post_srq_recv), built in its own buffer as QP.PostRecv builds
// a queue pair's.
func (s *SRQ) PostRecv(wrs ...RecvWR) error {
	if len(wrs) == 0 {
		return nil
	}
	s.recv.mu.Lock()
	defer s.recv.mu.Unlock()
	if s.srq == nil {
		return &OpError{Op: "ibv_post_srq_recv", Err: os.ErrClosed}
	}
	first := recvChain(&s.recv, wrs)
	rc, _ := s.postRecv.Int(caddr(s.srq), uintptr(unsafe.Pointer(first)), uintptr(unsafe.Pointer(&s.recv.bad)), 0, 0, 0)
	if rc != 0 {
		return opErrorRC("ibv_post_srq_recv", rc)
	}
	return nil
}

// Modify applies the attr fields selected by mask, as ibv_modify_srq does:
// SRQAttrMaxWR resizes the queue, which not every device can do, and
// SRQAttrLimit arms the SRQ_LIMIT_REACHED event, raised once fewer receives
// than the limit are left, after which the limit is 0 again.
func (s *SRQ) Modify(attr *SRQAttr, mask SRQAttrMask) error {
	if s.srq == nil {
		return &OpError{Op: "ibv_modify_srq", Err: os.ErrClosed}
	}
	f, err := load("ibv_modify_srq")
	if err != nil {
		return err
	}
	ca := srqAttrToC(*attr)
	if rc, _ := f.modifySRQ.Int(caddr(s.srq), uintptr(unsafe.Pointer(&ca)), cint(int32(mask)), 0, 0, 0); rc != 0 {
		return opErrorRC("ibv_modify_srq", rc)
	}
	return nil
}

// Query returns the attributes of the shared receive queue.
func (s *SRQ) Query() (*SRQAttr, error) {
	if s.srq == nil {
		return nil, &OpError{Op: "ibv_query_srq", Err: os.ErrClosed}
	}
	f, err := load("ibv_query_srq")
	if err != nil {
		return nil, err
	}
	var ca rdmaabi.IbvSrqAttr
	if rc, _ := f.querySRQ.Int(caddr(s.srq), uintptr(unsafe.Pointer(&ca)), 0, 0, 0, 0); rc != 0 {
		return nil, opErrorRC("ibv_query_srq", rc)
	}
	attr := srqAttrFromC(ca)
	return &attr, nil
}

// Num returns the number of the shared receive queue (ibv_get_srq_num),
// which an XRC send queue pair names in SendWR.RemoteSRQN to send to it. A
// device or library without it reports syscall.EOPNOTSUPP, which matches
// errors.ErrUnsupported.
func (s *SRQ) Num() (uint32, error) {
	if s.srq == nil {
		return 0, &OpError{Op: "ibv_get_srq_num", Err: os.ErrClosed}
	}
	get := verbsOp(uintptr(s.srq.Context), unsafe.Offsetof(rdmaabi.VerbsContext{}.Get_srq_num))
	if get.Addr() == 0 {
		return 0, &OpError{Op: "ibv_get_srq_num", Err: syscall.EOPNOTSUPP}
	}
	var n uint32
	if rc, _ := get.Int(caddr(s.srq), uintptr(unsafe.Pointer(&n)), 0, 0, 0, 0); rc != 0 {
		return 0, opErrorRC("ibv_get_srq_num", rc)
	}
	return n, nil
}

// Close destroys the shared receive queue, which fails with EBUSY while a
// queue pair still uses it. A second Close is a no-op.
func (s *SRQ) Close() error {
	s.recv.mu.Lock()
	defer s.recv.mu.Unlock()
	srq := s.srq
	if srq == nil {
		return nil
	}
	if s.owned {
		f, err := load("ibv_destroy_srq")
		if err != nil {
			return err
		}
		if rc, _ := f.destroySRQ.Int(caddr(srq), 0, 0, 0, 0, 0); rc != 0 {
			return opErrorRC("ibv_destroy_srq", rc)
		}
		srqs.Delete(caddr(srq))
	}
	s.srq = nil
	return nil
}

func srqAttrToC(a SRQAttr) rdmaabi.IbvSrqAttr {
	return rdmaabi.IbvSrqAttr{Max_wr: a.MaxWR, Max_sge: a.MaxSGE, Srq_limit: a.Limit}
}

func srqAttrFromC(ca rdmaabi.IbvSrqAttr) SRQAttr {
	return SRQAttr{MaxWR: ca.Max_wr, MaxSGE: ca.Max_sge, Limit: ca.Srq_limit}
}
