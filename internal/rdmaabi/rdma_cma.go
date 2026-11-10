package rdmaabi

// RdmaEventChannel is struct rdma_event_channel.
type RdmaEventChannel struct {
	Fd int32
}

// RdmaIbAddr is struct rdma_ib_addr.
type RdmaIbAddr struct {
	Sgid IbvGid
	Dgid IbvGid
	Pkey [2]byte // __be16
	_    [6]byte
}

// RdmaAddr is struct rdma_addr. Src_storage and Dst_storage are the
// struct sockaddr_storage members of the two address unions; src_addr,
// src_sin and src_sin6 (and their dst_ counterparts) alias their start.
type RdmaAddr struct {
	Src_storage [128]byte
	Dst_storage [128]byte
	Addr        struct {
		Ibaddr RdmaIbAddr
	}
}

// RdmaRoute is struct rdma_route.
type RdmaRoute struct {
	Addr      RdmaAddr
	Path_rec  uint64
	Num_paths int32
	_         [4]byte
}

// RdmaCmId is struct rdma_cm_id.
type RdmaCmId struct {
	Verbs           uint64
	Channel         uint64
	Context         uint64
	Qp              uint64
	Route           RdmaRoute
	Ps              uint32
	Port_num        uint8
	_               [3]byte
	Event           uint64
	Send_cq_channel uint64
	Send_cq         uint64
	Recv_cq_channel uint64
	Recv_cq         uint64
	Srq             uint64
	Pd              uint64
	Qp_type         uint32
	_               [4]byte
}

// RdmaConnParam is struct rdma_conn_param.
type RdmaConnParam struct {
	Private_data        uint64
	Private_data_len    uint8
	Responder_resources uint8
	Initiator_depth     uint8
	Flow_control        uint8
	Retry_count         uint8
	Rnr_retry_count     uint8
	Srq                 uint8
	_                   [1]byte
	Qp_num              uint32
	_                   [4]byte
}

// RdmaCmEvent is struct rdma_cm_event. Of the param union only the conn
// member is named; the ud member and (from rdma-core 66) arg share its
// space.
type RdmaCmEvent struct {
	Id        uint64
	Listen_id uint64
	Event     uint32
	Status    int32
	Param     struct {
		Conn RdmaConnParam
		_    [32]byte
	}
}
