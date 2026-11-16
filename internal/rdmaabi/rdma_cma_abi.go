package rdmaabi

// UcmaAbiCmdHdr is struct ucma_abi_cmd_hdr, the start of every command
// written to the kernel's rdma_cm device: the command, the size of the
// request after the header and the size of the response.
type UcmaAbiCmdHdr struct {
	Cmd uint32
	In  uint16
	Out uint16
}

// UcmaAbiCreateId is struct ucma_abi_create_id.
type UcmaAbiCreateId struct {
	Cmd      uint32
	In       uint16
	Out      uint16
	Uid      uint64
	Response uint64
	Ps       uint16
	Qp_type  uint8
	_        [5]byte
}

// UcmaAbiCreateIdResp is struct ucma_abi_create_id_resp.
type UcmaAbiCreateIdResp struct {
	Id uint32
}

// UcmaAbiDestroyId is struct ucma_abi_destroy_id.
type UcmaAbiDestroyId struct {
	Cmd      uint32
	In       uint16
	Out      uint16
	Response uint64
	Id       uint32
	_        [4]byte
}

// UcmaAbiDestroyIdResp is struct ucma_abi_destroy_id_resp.
type UcmaAbiDestroyIdResp struct {
	Events_reported uint32
}

// UcmaAbiBind is struct ucma_abi_bind; Addr is its struct sockaddr_storage.
type UcmaAbiBind struct {
	Cmd       uint32
	In        uint16
	Out       uint16
	Id        uint32
	Addr_size uint16
	_         [2]byte
	Addr      [128]byte
}

// UcmaAbiResolveAddr is struct ucma_abi_resolve_addr; Src_addr and Dst_addr
// are its struct sockaddr_storage members.
type UcmaAbiResolveAddr struct {
	Cmd        uint32
	In         uint16
	Out        uint16
	Id         uint32
	Timeout_ms uint32
	Src_size   uint16
	Dst_size   uint16
	_          [4]byte
	Src_addr   [128]byte
	Dst_addr   [128]byte
}

// UcmaAbiResolveRoute is struct ucma_abi_resolve_route.
type UcmaAbiResolveRoute struct {
	Cmd        uint32
	In         uint16
	Out        uint16
	Id         uint32
	Timeout_ms uint32
}

// UcmaAbiQuery is struct ucma_abi_query.
type UcmaAbiQuery struct {
	Cmd      uint32
	In       uint16
	Out      uint16
	Response uint64
	Id       uint32
	Option   uint32
}

// UcmaAbiQueryAddrResp is struct ucma_abi_query_addr_resp, the response to
// a query with the option UCMA_QUERY_ADDR.
type UcmaAbiQueryAddrResp struct {
	Node_guid   [8]byte // __be64
	Port_num    uint8
	_           [1]byte
	Pkey        [2]byte // __be16
	Src_size    uint16
	Dst_size    uint16
	Src_addr    [128]byte
	Dst_addr    [128]byte
	Ibdev_index uint32
	_           [4]byte
}

// UcmaAbiConnParam is struct ucma_abi_conn_param. The member after qp_num
// is reserved here and the qkey of the kernel's header, which only AF_IB
// addresses use.
type UcmaAbiConnParam struct {
	Qp_num              uint32
	_                   [4]byte
	Private_data        [256]byte
	Private_data_len    uint8
	Srq                 uint8
	Responder_resources uint8
	Initiator_depth     uint8
	Flow_control        uint8
	Retry_count         uint8
	Rnr_retry_count     uint8
	Valid               uint8
}

// UcmaAbiUdParam is struct ucma_abi_ud_param.
type UcmaAbiUdParam struct {
	Qp_num           uint32
	Qkey             uint32
	Ah_attr          IbUverbsAhAttr
	Private_data     [256]byte
	Private_data_len uint8
	_                [11]byte
}

// UcmaAbiEce is struct ucma_abi_ece.
type UcmaAbiEce struct {
	Vendor_id uint32
	Attr_mod  uint32
}

// UcmaAbiConnect is struct ucma_abi_connect.
type UcmaAbiConnect struct {
	Cmd        uint32
	In         uint16
	Out        uint16
	Conn_param UcmaAbiConnParam
	Id         uint32
	_          [4]byte
	Ece        UcmaAbiEce
}

// UcmaAbiListen is struct ucma_abi_listen.
type UcmaAbiListen struct {
	Cmd     uint32
	In      uint16
	Out     uint16
	Id      uint32
	Backlog uint32
}

// UcmaAbiAccept is struct ucma_abi_accept.
type UcmaAbiAccept struct {
	Cmd        uint32
	In         uint16
	Out        uint16
	Uid        uint64
	Conn_param UcmaAbiConnParam
	Id         uint32
	_          [4]byte
	Ece        UcmaAbiEce
}

// UcmaAbiReject is struct ucma_abi_reject.
type UcmaAbiReject struct {
	Cmd              uint32
	In               uint16
	Out              uint16
	Id               uint32
	Private_data_len uint8
	Reason           uint8
	_                [2]byte
	Private_data     [256]byte
}

// UcmaAbiDisconnect is struct ucma_abi_disconnect.
type UcmaAbiDisconnect struct {
	Cmd uint32
	In  uint16
	Out uint16
	Id  uint32
}

// UcmaAbiInitQpAttr is struct ucma_abi_init_qp_attr, whose response is a
// struct ib_uverbs_qp_attr.
type UcmaAbiInitQpAttr struct {
	Cmd      uint32
	In       uint16
	Out      uint16
	Response uint64
	Id       uint32
	Qp_state uint32
}

// UcmaAbiGetEvent is struct ucma_abi_get_event.
type UcmaAbiGetEvent struct {
	Cmd      uint32
	In       uint16
	Out      uint16
	Response uint64
}

// UcmaAbiEventResp is struct ucma_abi_event_resp. Of the param union only
// the conn member is named; the ud member, the longer one, starts at the
// same byte, and so does arg32.
type UcmaAbiEventResp struct {
	Uid    uint64
	Id     uint32
	Event  uint32
	Status uint32
	Param  struct {
		Conn UcmaAbiConnParam
		_    [36]byte
	}
	Ece UcmaAbiEce
}

// UcmaAbiMigrateId is struct ucma_abi_migrate_id.
type UcmaAbiMigrateId struct {
	Cmd      uint32
	In       uint16
	Out      uint16
	Response uint64
	Id       uint32
	Fd       uint32
}

// UcmaAbiMigrateResp is struct ucma_abi_migrate_resp.
type UcmaAbiMigrateResp struct {
	Events_reported uint32
}

// IbUverbsGlobalRoute is struct ib_uverbs_global_route.
type IbUverbsGlobalRoute struct {
	Dgid          [16]byte
	Flow_label    uint32
	Sgid_index    uint8
	Hop_limit     uint8
	Traffic_class uint8
	_             [1]byte
}

// IbUverbsAhAttr is struct ib_uverbs_ah_attr.
type IbUverbsAhAttr struct {
	Grh           IbUverbsGlobalRoute
	Dlid          uint16
	Sl            uint8
	Src_path_bits uint8
	Static_rate   uint8
	Is_global     uint8
	Port_num      uint8
	_             [1]byte
}

// IbUverbsQpAttr is struct ib_uverbs_qp_attr, the queue-pair attributes in
// the kernel's form.
type IbUverbsQpAttr struct {
	Qp_attr_mask        uint32
	Qp_state            uint32
	Cur_qp_state        uint32
	Path_mtu            uint32
	Path_mig_state      uint32
	Qkey                uint32
	Rq_psn              uint32
	Sq_psn              uint32
	Dest_qp_num         uint32
	Qp_access_flags     uint32
	Ah_attr             IbUverbsAhAttr
	Alt_ah_attr         IbUverbsAhAttr
	Max_send_wr         uint32
	Max_recv_wr         uint32
	Max_send_sge        uint32
	Max_recv_sge        uint32
	Max_inline_data     uint32
	Pkey_index          uint16
	Alt_pkey_index      uint16
	En_sqd_async_notify uint8
	Sq_draining         uint8
	Max_rd_atomic       uint8
	Max_dest_rd_atomic  uint8
	Min_rnr_timer       uint8
	Port_num            uint8
	Timeout             uint8
	Retry_cnt           uint8
	Rnr_retry           uint8
	Alt_port_num        uint8
	Alt_timeout         uint8
	_                   [5]byte
}
