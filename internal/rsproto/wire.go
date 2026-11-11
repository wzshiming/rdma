package rsproto

// Sizes and limits of the protocol (rsocket.c lines 65-72).
const (
	OLAPStartSize = 2048   // RS_OLAP_START_SIZE: the first message of a send, each next one doubling up to MaxTransfer
	MaxTransfer   = 65536  // RS_MAX_TRANSFER: the largest data message
	SndLowat      = 2048   // RS_SNDLOWAT: the free send buffer a sender waits for
	QPMinSize     = 16     // RS_QP_MIN_SIZE: the least send or receive queue depth
	QPMaxSize     = 0xFFFE // RS_QP_MAX_SIZE: the greatest send or receive queue depth
	QPCtrlSize    = 4      // RS_QP_CTRL_SIZE: grant, credit and control messages in flight at once
	SGLSize       = 2      // RS_SGL_SIZE: the entries of a grant table
)

// Stock defaults of rsockets (rsocket.c lines 139-144), which the files
// under /etc/rdma/rsocket override for a C process.
const (
	DefaultSendBufferSize    = 1 << 17 // def_wmem
	DefaultReceiveBufferSize = 1 << 17 // def_mem
	DefaultSendQueueSize     = 384     // def_sqsize
	DefaultReceiveQueueSize  = 384     // def_rqsize
	DefaultMaxInline         = 64      // def_inline
)
