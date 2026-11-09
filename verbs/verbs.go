package verbs

import (
	"net/netip"
	"strconv"
)

// OpError is the error returned by every failing rdma-core call: Op names the
// libibverbs function and Err is the syscall.Errno it produced. On platforms
// without libibverbs Err is errors.ErrUnsupported.
type OpError struct {
	Op  string
	Err error
}

func (e *OpError) Error() string { return "verbs: " + e.Op + ": " + e.Err.Error() }

// Unwrap returns the underlying error.
func (e *OpError) Unwrap() error { return e.Err }

// WCError is the error returned by WC.Err for a work completion whose status
// is not WCSuccess.
type WCError struct {
	Status    WCStatus
	VendorErr uint32
}

func (e *WCError) Error() string {
	return "verbs: work completion: " + e.Status.String() + " (vendor error 0x" + strconv.FormatUint(uint64(e.VendorErr), 16) + ")"
}

// GID is a 16-byte global identifier; RoCE GIDs are IPv6 addresses or
// IPv4-mapped IPv6 addresses.
type GID [16]byte

// String formats the GID as an IPv6 address.
func (g GID) String() string { return netip.AddrFrom16(g).String() }

// IsZero reports whether every byte of the GID is zero.
func (g GID) IsZero() bool { return g == GID{} }

// MTU is a path MTU value (IBV_MTU_*).
type MTU int

// Path MTU values.
const (
	MTU256  MTU = 1
	MTU512  MTU = 2
	MTU1024 MTU = 3
	MTU2048 MTU = 4
	MTU4096 MTU = 5
)

// Bytes returns the MTU in bytes, or 0 for an unknown value.
func (m MTU) Bytes() int {
	if m < MTU256 || m > MTU4096 {
		return 0
	}
	return 128 << uint(m)
}

func (m MTU) String() string {
	if b := m.Bytes(); b != 0 {
		return strconv.Itoa(b)
	}
	return unknown("MTU", int(m))
}

// PortState is the logical state of a port (IBV_PORT_*).
type PortState int

// Port states.
const (
	PortNop         PortState = 0
	PortDown        PortState = 1
	PortInit        PortState = 2
	PortArmed       PortState = 3
	PortActive      PortState = 4
	PortActiveDefer PortState = 5
)

var portStateNames = []string{"NOP", "DOWN", "INIT", "ARMED", "ACTIVE", "ACTIVE_DEFER"}

func (s PortState) String() string { return enumString("PortState", int(s), portStateNames) }

// LinkLayer is the link layer of a port (IBV_LINK_LAYER_*).
type LinkLayer int

// Link layers.
const (
	LinkLayerUnspecified LinkLayer = 0
	LinkLayerInfiniBand  LinkLayer = 1
	LinkLayerEthernet    LinkLayer = 2
)

var linkLayerNames = []string{"Unspecified", "InfiniBand", "Ethernet"}

func (l LinkLayer) String() string { return enumString("LinkLayer", int(l), linkLayerNames) }

// GIDType is the type of a GID table entry (IBV_GID_TYPE_*).
type GIDType int

// GID types.
const (
	GIDTypeIB     GIDType = 0
	GIDTypeRoCEv1 GIDType = 1
	GIDTypeRoCEv2 GIDType = 2
)

var gidTypeNames = []string{"IB", "RoCE v1", "RoCE v2"}

func (t GIDType) String() string { return enumString("GIDType", int(t), gidTypeNames) }

// AccessFlags are memory access permissions (IBV_ACCESS_*).
type AccessFlags uint32

// Access flags.
const (
	AccessLocalWrite      AccessFlags = 1 << 0
	AccessRemoteWrite     AccessFlags = 1 << 1
	AccessRemoteRead      AccessFlags = 1 << 2
	AccessRemoteAtomic    AccessFlags = 1 << 3
	AccessMWBind          AccessFlags = 1 << 4
	AccessZeroBased       AccessFlags = 1 << 5
	AccessOnDemand        AccessFlags = 1 << 6
	AccessHugeTLB         AccessFlags = 1 << 7
	AccessFlushGlobal     AccessFlags = 1 << 8
	AccessFlushPersistent AccessFlags = 1 << 9
	AccessRelaxedOrdering AccessFlags = 1 << 20
)

var accessFlagNames = []flagName{
	{uint64(AccessLocalWrite), "LOCAL_WRITE"},
	{uint64(AccessRemoteWrite), "REMOTE_WRITE"},
	{uint64(AccessRemoteRead), "REMOTE_READ"},
	{uint64(AccessRemoteAtomic), "REMOTE_ATOMIC"},
	{uint64(AccessMWBind), "MW_BIND"},
	{uint64(AccessZeroBased), "ZERO_BASED"},
	{uint64(AccessOnDemand), "ON_DEMAND"},
	{uint64(AccessHugeTLB), "HUGETLB"},
	{uint64(AccessFlushGlobal), "FLUSH_GLOBAL"},
	{uint64(AccessFlushPersistent), "FLUSH_PERSISTENT"},
	{uint64(AccessRelaxedOrdering), "RELAXED_ORDERING"},
}

func (f AccessFlags) String() string { return flagString(uint64(f), accessFlagNames) }

// QPType is the transport service type of a queue pair (IBV_QPT_*).
type QPType int

// Queue pair types.
const (
	QPTypeRC        QPType = 2
	QPTypeUC        QPType = 3
	QPTypeUD        QPType = 4
	QPTypeRawPacket QPType = 8
	QPTypeXRCSend   QPType = 9
	QPTypeXRCRecv   QPType = 10
	QPTypeDriver    QPType = 0xff
)

var qpTypeNames = map[QPType]string{
	QPTypeRC:        "RC",
	QPTypeUC:        "UC",
	QPTypeUD:        "UD",
	QPTypeRawPacket: "RAW_PACKET",
	QPTypeXRCSend:   "XRC_SEND",
	QPTypeXRCRecv:   "XRC_RECV",
	QPTypeDriver:    "DRIVER",
}

func (t QPType) String() string {
	if s, ok := qpTypeNames[t]; ok {
		return s
	}
	return unknown("QPType", int(t))
}

// QPState is the state of a queue pair (IBV_QPS_*).
type QPState int

// Queue pair states.
const (
	QPStateReset   QPState = 0
	QPStateInit    QPState = 1
	QPStateRTR     QPState = 2
	QPStateRTS     QPState = 3
	QPStateSQD     QPState = 4
	QPStateSQE     QPState = 5
	QPStateErr     QPState = 6
	QPStateUnknown QPState = 7
)

var qpStateNames = []string{"RESET", "INIT", "RTR", "RTS", "SQD", "SQE", "ERR", "UNKNOWN"}

func (s QPState) String() string { return enumString("QPState", int(s), qpStateNames) }

// MigState is the path migration state of a queue pair (IBV_MIG_*).
type MigState int

// Path migration states.
const (
	MigMigrated MigState = 0
	MigRearm    MigState = 1
	MigArmed    MigState = 2
)

var migStateNames = []string{"MIGRATED", "REARM", "ARMED"}

func (s MigState) String() string { return enumString("MigState", int(s), migStateNames) }

// QPAttrMask selects the QPAttr fields that QP.Modify applies and QP.Query
// reads (IBV_QP_*).
type QPAttrMask uint32

// Queue pair attribute mask bits.
const (
	QPAttrState            QPAttrMask = 1 << 0
	QPAttrCurState         QPAttrMask = 1 << 1
	QPAttrEnSQDAsyncNotify QPAttrMask = 1 << 2
	QPAttrAccessFlags      QPAttrMask = 1 << 3
	QPAttrPkeyIndex        QPAttrMask = 1 << 4
	QPAttrPort             QPAttrMask = 1 << 5
	QPAttrQKey             QPAttrMask = 1 << 6
	QPAttrAV               QPAttrMask = 1 << 7
	QPAttrPathMTU          QPAttrMask = 1 << 8
	QPAttrTimeout          QPAttrMask = 1 << 9
	QPAttrRetryCnt         QPAttrMask = 1 << 10
	QPAttrRNRRetry         QPAttrMask = 1 << 11
	QPAttrRQPSN            QPAttrMask = 1 << 12
	QPAttrMaxQPRDAtomic    QPAttrMask = 1 << 13
	QPAttrAltPath          QPAttrMask = 1 << 14
	QPAttrMinRNRTimer      QPAttrMask = 1 << 15
	QPAttrSQPSN            QPAttrMask = 1 << 16
	QPAttrMaxDestRDAtomic  QPAttrMask = 1 << 17
	QPAttrPathMigState     QPAttrMask = 1 << 18
	QPAttrCap              QPAttrMask = 1 << 19
	QPAttrDestQPN          QPAttrMask = 1 << 20
	QPAttrRateLimit        QPAttrMask = 1 << 25
)

var qpAttrMaskNames = []flagName{
	{uint64(QPAttrState), "STATE"},
	{uint64(QPAttrCurState), "CUR_STATE"},
	{uint64(QPAttrEnSQDAsyncNotify), "EN_SQD_ASYNC_NOTIFY"},
	{uint64(QPAttrAccessFlags), "ACCESS_FLAGS"},
	{uint64(QPAttrPkeyIndex), "PKEY_INDEX"},
	{uint64(QPAttrPort), "PORT"},
	{uint64(QPAttrQKey), "QKEY"},
	{uint64(QPAttrAV), "AV"},
	{uint64(QPAttrPathMTU), "PATH_MTU"},
	{uint64(QPAttrTimeout), "TIMEOUT"},
	{uint64(QPAttrRetryCnt), "RETRY_CNT"},
	{uint64(QPAttrRNRRetry), "RNR_RETRY"},
	{uint64(QPAttrRQPSN), "RQ_PSN"},
	{uint64(QPAttrMaxQPRDAtomic), "MAX_QP_RD_ATOMIC"},
	{uint64(QPAttrAltPath), "ALT_PATH"},
	{uint64(QPAttrMinRNRTimer), "MIN_RNR_TIMER"},
	{uint64(QPAttrSQPSN), "SQ_PSN"},
	{uint64(QPAttrMaxDestRDAtomic), "MAX_DEST_RD_ATOMIC"},
	{uint64(QPAttrPathMigState), "PATH_MIG_STATE"},
	{uint64(QPAttrCap), "CAP"},
	{uint64(QPAttrDestQPN), "DEST_QPN"},
	{uint64(QPAttrRateLimit), "RATE_LIMIT"},
}

func (m QPAttrMask) String() string { return flagString(uint64(m), qpAttrMaskNames) }

// WROpcode is the operation of a send work request (IBV_WR_*).
type WROpcode int

// Send work request opcodes.
const (
	WRWrite    WROpcode = 0 // RDMA Write
	WRWriteImm WROpcode = 1 // RDMA Write with immediate data
	WRSend     WROpcode = 2 // Send
	WRSendImm  WROpcode = 3 // Send with immediate data
	WRRead     WROpcode = 4 // RDMA Read
)

var wrOpcodeNames = []string{"RDMA_WRITE", "RDMA_WRITE_WITH_IMM", "SEND", "SEND_WITH_IMM", "RDMA_READ"}

func (op WROpcode) String() string { return enumString("WROpcode", int(op), wrOpcodeNames) }

// SendFlags are the flags of a send work request (IBV_SEND_*).
type SendFlags uint32

// Send flags.
const (
	SendFence     SendFlags = 1 << 0
	SendSignaled  SendFlags = 1 << 1
	SendSolicited SendFlags = 1 << 2
	SendInline    SendFlags = 1 << 3
	SendIPCsum    SendFlags = 1 << 4
)

var sendFlagNames = []flagName{
	{uint64(SendFence), "FENCE"},
	{uint64(SendSignaled), "SIGNALED"},
	{uint64(SendSolicited), "SOLICITED"},
	{uint64(SendInline), "INLINE"},
	{uint64(SendIPCsum), "IP_CSUM"},
}

func (f SendFlags) String() string { return flagString(uint64(f), sendFlagNames) }

// WCStatus is the status of a work completion (IBV_WC_*).
type WCStatus int

// Work completion statuses.
const (
	WCSuccess          WCStatus = 0
	WCLocLenErr        WCStatus = 1
	WCLocQPOpErr       WCStatus = 2
	WCLocEECOpErr      WCStatus = 3
	WCLocProtErr       WCStatus = 4
	WCWRFlushErr       WCStatus = 5
	WCMWBindErr        WCStatus = 6
	WCBadRespErr       WCStatus = 7
	WCLocAccessErr     WCStatus = 8
	WCRemInvReqErr     WCStatus = 9
	WCRemAccessErr     WCStatus = 10
	WCRemOpErr         WCStatus = 11
	WCRetryExcErr      WCStatus = 12
	WCRNRRetryExcErr   WCStatus = 13
	WCLocRDDViolErr    WCStatus = 14
	WCRemInvRDReqErr   WCStatus = 15
	WCRemAbortErr      WCStatus = 16
	WCInvEECNErr       WCStatus = 17
	WCInvEECStateErr   WCStatus = 18
	WCFatalErr         WCStatus = 19
	WCRespTimeoutErr   WCStatus = 20
	WCGeneralErr       WCStatus = 21
	WCTMErr            WCStatus = 22
	WCTMRndvIncomplete WCStatus = 23
)

// wcStatusNames carries the texts of ibv_wc_status_str so that they are
// available on every platform.
var wcStatusNames = []string{
	"success",
	"local length error",
	"local QP operation error",
	"local EE context operation error",
	"local protection error",
	"Work Request Flushed Error",
	"memory management operation error",
	"bad response error",
	"local access error",
	"remote invalid request error",
	"remote access error",
	"remote operation error",
	"transport retry counter exceeded",
	"RNR retry counter exceeded",
	"local RDD violation error",
	"remote invalid RD request",
	"aborted error",
	"invalid EE context number",
	"invalid EE context state",
	"fatal error",
	"response timeout error",
	"general error",
	"TM error",
	"TM software rendezvous",
}

func (s WCStatus) String() string { return enumString("WCStatus", int(s), wcStatusNames) }

// WCOpcode is the operation a work completion reports (IBV_WC_*).
type WCOpcode int

// Work completion opcodes.
const (
	WCOpSend            WCOpcode = 0
	WCOpRDMAWrite       WCOpcode = 1
	WCOpRDMARead        WCOpcode = 2
	WCOpCompSwap        WCOpcode = 3
	WCOpFetchAdd        WCOpcode = 4
	WCOpBindMW          WCOpcode = 5
	WCOpLocalInv        WCOpcode = 6
	WCOpTSO             WCOpcode = 7
	WCOpFlush           WCOpcode = 8
	WCOpAtomicWrite     WCOpcode = 9
	WCOpRecv            WCOpcode = 1 << 7
	WCOpRecvRDMAWithImm WCOpcode = 1<<7 + 1
	WCOpTMAdd           WCOpcode = 1<<7 + 2
	WCOpTMDel           WCOpcode = 1<<7 + 3
	WCOpTMSync          WCOpcode = 1<<7 + 4
	WCOpTMRecv          WCOpcode = 1<<7 + 5
	WCOpTMNoTag         WCOpcode = 1<<7 + 6
	WCOpDriver1         WCOpcode = 1<<7 + 7
	WCOpDriver2         WCOpcode = 1<<7 + 8
	WCOpDriver3         WCOpcode = 1<<7 + 9
)

var wcOpcodeNames = map[WCOpcode]string{
	WCOpSend:            "SEND",
	WCOpRDMAWrite:       "RDMA_WRITE",
	WCOpRDMARead:        "RDMA_READ",
	WCOpCompSwap:        "COMP_SWAP",
	WCOpFetchAdd:        "FETCH_ADD",
	WCOpBindMW:          "BIND_MW",
	WCOpLocalInv:        "LOCAL_INV",
	WCOpTSO:             "TSO",
	WCOpFlush:           "FLUSH",
	WCOpAtomicWrite:     "ATOMIC_WRITE",
	WCOpRecv:            "RECV",
	WCOpRecvRDMAWithImm: "RECV_RDMA_WITH_IMM",
	WCOpTMAdd:           "TM_ADD",
	WCOpTMDel:           "TM_DEL",
	WCOpTMSync:          "TM_SYNC",
	WCOpTMRecv:          "TM_RECV",
	WCOpTMNoTag:         "TM_NO_TAG",
	WCOpDriver1:         "DRIVER1",
	WCOpDriver2:         "DRIVER2",
	WCOpDriver3:         "DRIVER3",
}

func (op WCOpcode) String() string {
	if s, ok := wcOpcodeNames[op]; ok {
		return s
	}
	return unknown("WCOpcode", int(op))
}

// WCFlags are the flags of a work completion (IBV_WC_*).
type WCFlags uint32

// Work completion flags.
const (
	WCGRH         WCFlags = 1 << 0
	WCWithImm     WCFlags = 1 << 1
	WCIPCsumOK    WCFlags = 1 << 2
	WCWithInv     WCFlags = 1 << 3
	WCTMSyncReq   WCFlags = 1 << 4
	WCTMMatch     WCFlags = 1 << 5
	WCTMDataValid WCFlags = 1 << 6
)

var wcFlagNames = []flagName{
	{uint64(WCGRH), "GRH"},
	{uint64(WCWithImm), "WITH_IMM"},
	{uint64(WCIPCsumOK), "IP_CSUM_OK"},
	{uint64(WCWithInv), "WITH_INV"},
	{uint64(WCTMSyncReq), "TM_SYNC_REQ"},
	{uint64(WCTMMatch), "TM_MATCH"},
	{uint64(WCTMDataValid), "TM_DATA_VALID"},
}

func (f WCFlags) String() string { return flagString(uint64(f), wcFlagNames) }

// EventType is the type of an asynchronous event (IBV_EVENT_*).
type EventType int

// Asynchronous event types.
const (
	EventCQErr            EventType = 0
	EventQPFatal          EventType = 1
	EventQPReqErr         EventType = 2
	EventQPAccessErr      EventType = 3
	EventCommEst          EventType = 4
	EventSQDrained        EventType = 5
	EventPathMig          EventType = 6
	EventPathMigErr       EventType = 7
	EventDeviceFatal      EventType = 8
	EventPortActive       EventType = 9
	EventPortErr          EventType = 10
	EventLIDChange        EventType = 11
	EventPkeyChange       EventType = 12
	EventSMChange         EventType = 13
	EventSRQErr           EventType = 14
	EventSRQLimitReached  EventType = 15
	EventQPLastWQEReached EventType = 16
	EventClientReregister EventType = 17
	EventGIDChange        EventType = 18
	EventWQFatal          EventType = 19
)

var eventTypeNames = []string{
	"CQ_ERR", "QP_FATAL", "QP_REQ_ERR", "QP_ACCESS_ERR", "COMM_EST", "SQ_DRAINED",
	"PATH_MIG", "PATH_MIG_ERR", "DEVICE_FATAL", "PORT_ACTIVE", "PORT_ERR", "LID_CHANGE",
	"PKEY_CHANGE", "SM_CHANGE", "SRQ_ERR", "SRQ_LIMIT_REACHED", "QP_LAST_WQE_REACHED",
	"CLIENT_REREGISTER", "GID_CHANGE", "WQ_FATAL",
}

func (t EventType) String() string { return enumString("EventType", int(t), eventTypeNames) }

// DeviceAttr holds the attributes and limits of a device (struct ibv_device_attr).
type DeviceAttr struct {
	FWVer           string
	NodeGUID        uint64
	SysImageGUID    uint64
	MaxMRSize       uint64
	PageSizeCap     uint64
	VendorID        uint32
	VendorPartID    uint32
	HWVer           uint32
	MaxQP           int
	MaxQPWR         int
	DeviceCapFlags  uint32
	MaxSGE          int
	MaxSGERD        int
	MaxCQ           int
	MaxCQE          int
	MaxMR           int
	MaxPD           int
	MaxQPRDAtom     int
	MaxResRDAtom    int
	MaxQPInitRDAtom int
	MaxMW           int
	MaxAH           int
	MaxSRQ          int
	MaxSRQWR        int
	MaxSRQSGE       int
	MaxPkeys        uint16
	LocalCAAckDelay uint8
	PhysPortCnt     uint8
}

// PortAttr holds the attributes of a port (struct ibv_port_attr).
type PortAttr struct {
	State         PortState
	MaxMTU        MTU
	ActiveMTU     MTU
	GIDTableLen   int
	PortCapFlags  uint32
	MaxMsgSize    uint32
	BadPkeyCntr   uint32
	QKeyViolCntr  uint32
	PkeyTableLen  uint16
	LID           uint16
	SMLID         uint16
	LMC           uint8
	MaxVLNum      uint8
	SMSL          uint8
	SubnetTimeout uint8
	InitTypeReply uint8
	ActiveWidth   uint8
	ActiveSpeed   uint8
	PhysState     uint8
	LinkLayer     LinkLayer
}

// GIDEntry is a populated GID table entry.
type GIDEntry struct {
	Index int
	Type  GIDType
	GID   GID
}

// QPCap holds the capacities of a queue pair (struct ibv_qp_cap).
type QPCap struct {
	MaxSendWR     uint32
	MaxRecvWR     uint32
	MaxSendSGE    uint32
	MaxRecvSGE    uint32
	MaxInlineData uint32
}

// QPInitAttr describes a queue pair to create (struct ibv_qp_init_attr).
// PD.CreateQP overwrites Cap with the capacities actually granted, which is
// how libibverbs reports the maximum inline size.
type QPInitAttr struct {
	SendCQ    *CQ
	RecvCQ    *CQ
	Type      QPType
	Cap       QPCap
	SignalAll bool // every send work request generates a completion
}

// GlobalRoute is the GRH part of an address (struct ibv_global_route).
type GlobalRoute struct {
	DGID         GID
	FlowLabel    uint32
	SGIDIndex    uint8
	HopLimit     uint8
	TrafficClass uint8
}

// AHAttr is an address vector (struct ibv_ah_attr).
type AHAttr struct {
	GRH         GlobalRoute
	DLID        uint16
	SL          uint8
	SrcPathBits uint8
	StaticRate  uint8
	IsGlobal    bool
	Port        uint8
}

// QPAttr holds queue pair attributes for QP.Modify and QP.Query
// (struct ibv_qp_attr).
type QPAttr struct {
	State            QPState
	CurState         QPState
	PathMTU          MTU
	PathMigState     MigState
	QKey             uint32
	RQPSN            uint32
	SQPSN            uint32
	DestQPNum        uint32
	AccessFlags      AccessFlags
	Cap              QPCap
	AH               AHAttr
	AltAH            AHAttr
	PkeyIndex        uint16
	AltPkeyIndex     uint16
	EnSQDAsyncNotify bool
	SQDraining       bool
	MaxRDAtomic      uint8
	MaxDestRDAtomic  uint8
	MinRNRTimer      uint8
	Port             uint8
	Timeout          uint8
	RetryCnt         uint8
	RNRRetry         uint8
	AltPort          uint8
	AltTimeout       uint8
	RateLimit        uint32
}

// SGE is a scatter/gather element (struct ibv_sge); build one with MR.SGE.
type SGE struct {
	Addr   uint64
	Length uint32
	LKey   uint32
}

// SendWR is a send work request (struct ibv_send_wr) for an RC or UC queue
// pair. Imm is passed through unchanged and read back from WC.Imm on the
// remote side; RemoteAddr and RKey are used by the RDMA operations.
type SendWR struct {
	ID         uint64
	Op         WROpcode
	SGE        []SGE
	Flags      SendFlags
	Imm        uint32
	RemoteAddr uint64
	RKey       uint32
}

// RecvWR is a receive work request (struct ibv_recv_wr).
type RecvWR struct {
	ID  uint64
	SGE []SGE
}

// WC is a work completion (struct ibv_wc).
type WC struct {
	ID           uint64
	Status       WCStatus
	Opcode       WCOpcode
	VendorErr    uint32
	Len          uint32
	Imm          uint32 // valid when HasImm reports true
	QPNum        uint32
	SrcQP        uint32
	Flags        WCFlags
	PkeyIndex    uint16
	SLID         uint16
	SL           uint8
	DLIDPathBits uint8
}

// HasImm reports whether the completion carries immediate data.
func (wc WC) HasImm() bool { return wc.Flags&WCWithImm != 0 }

// Err returns nil for a successful completion and a *WCError otherwise.
func (wc WC) Err() error {
	if wc.Status == WCSuccess {
		return nil
	}
	return &WCError{Status: wc.Status, VendorErr: wc.VendorErr}
}

// AsyncEvent is an asynchronous event (struct ibv_async_event). QP, CQ or Port
// identifies the affected object according to Type; the others are zero.
type AsyncEvent struct {
	Type EventType
	QP   *QP
	CQ   *CQ
	Port uint8
}

type flagName struct {
	bit  uint64
	name string
}

func flagString(v uint64, names []flagName) string {
	if v == 0 {
		return "0"
	}
	var s string
	for _, n := range names {
		if v&n.bit != 0 {
			if s != "" {
				s += "|"
			}
			s += n.name
			v &^= n.bit
		}
	}
	if v != 0 {
		if s != "" {
			s += "|"
		}
		s += "0x" + strconv.FormatUint(v, 16)
	}
	return s
}

func enumString(typ string, v int, names []string) string {
	if v >= 0 && v < len(names) {
		return names[v]
	}
	return unknown(typ, v)
}

func unknown(typ string, v int) string { return typ + "(" + strconv.Itoa(v) + ")" }
