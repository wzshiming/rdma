package rdmaabi

// IbvDevice is the leading part of struct ibv_device, up to transport_type.
type IbvDevice struct {
	_              [2]uint64 // _ops: two function pointers
	Node_type      int32
	Transport_type int32
}

// IbvDeviceAttr is struct ibv_device_attr, filled by ibv_query_device.
type IbvDeviceAttr struct {
	Fw_ver                    [64]byte
	Node_guid                 [8]byte // __be64
	Sys_image_guid            [8]byte // __be64
	Max_mr_size               uint64
	Page_size_cap             uint64
	Vendor_id                 uint32
	Vendor_part_id            uint32
	Hw_ver                    uint32
	Max_qp                    int32
	Max_qp_wr                 int32
	Device_cap_flags          uint32
	Max_sge                   int32
	Max_sge_rd                int32
	Max_cq                    int32
	Max_cqe                   int32
	Max_mr                    int32
	Max_pd                    int32
	Max_qp_rd_atom            int32
	Max_ee_rd_atom            int32
	Max_res_rd_atom           int32
	Max_qp_init_rd_atom       int32
	Max_ee_init_rd_atom       int32
	Atomic_cap                uint32
	Max_ee                    int32
	Max_rdd                   int32
	Max_mw                    int32
	Max_raw_ipv6_qp           int32
	Max_raw_ethy_qp           int32
	Max_mcast_grp             int32
	Max_mcast_qp_attach       int32
	Max_total_mcast_qp_attach int32
	Max_ah                    int32
	Max_fmr                   int32
	Max_map_per_fmr           int32
	Max_srq                   int32
	Max_srq_wr                int32
	Max_srq_sge               int32
	Max_pkeys                 uint16
	Local_ca_ack_delay        uint8
	Phys_port_cnt             uint8
	_                         [4]byte
}

// IbvPortAttr is struct ibv_port_attr, filled by ibv_query_port.
type IbvPortAttr struct {
	State           uint32
	Max_mtu         uint32
	Active_mtu      uint32
	Gid_tbl_len     int32
	Port_cap_flags  uint32
	Max_msg_sz      uint32
	Bad_pkey_cntr   uint32
	Qkey_viol_cntr  uint32
	Pkey_tbl_len    uint16
	Lid             uint16
	Sm_lid          uint16
	Lmc             uint8
	Max_vl_num      uint8
	Sm_sl           uint8
	Subnet_timeout  uint8
	Init_type_reply uint8
	Active_width    uint8
	Active_speed    uint8
	Phys_state      uint8
	Link_layer      uint8
	Flags           uint8
	Port_cap_flags2 uint16
	Active_speed_ex uint32
}

// IbvGid is union ibv_gid: a GID in network byte order.
type IbvGid [16]byte

// IbvGidEntry is struct ibv_gid_entry, an element of the array
// _ibv_query_gid_table fills.
type IbvGidEntry struct {
	Gid          IbvGid
	Gid_index    uint32
	Port_num     uint32
	Gid_type     uint32
	Ndev_ifindex uint32
}

// IbvContextOps is struct ibv_context_ops, the table of provider functions
// the header-inline verbs call; the slots the bindings use are named.
type IbvContextOps struct {
	_             [11]uint64
	Poll_cq       uint64
	Req_notify_cq uint64
	_             [12]uint64
	Post_send     uint64
	Post_recv     uint64
	_             [5]uint64
}

// IbvContext is the leading part of struct ibv_context, up to the mutex.
type IbvContext struct {
	Device           uint64
	Ops              IbvContextOps
	Cmd_fd           int32
	Async_fd         int32
	Num_comp_vectors int32
}

// IbvCompChannel is struct ibv_comp_channel.
type IbvCompChannel struct {
	Context uint64
	Fd      int32
	Refcnt  int32
}

// IbvAsyncEvent is struct ibv_async_event, filled by ibv_get_async_event.
// Element is the union of the cq, qp, srq and wq pointers and the int
// port_num, all starting at its first byte.
type IbvAsyncEvent struct {
	Element    [8]byte
	Event_type uint32
	_          [4]byte
}

// IbvWc is struct ibv_wc, an element of the array poll_cq fills.
type IbvWc struct {
	Wr_id          uint64
	Status         uint32
	Opcode         uint32
	Vendor_err     uint32
	Byte_len       uint32
	Imm_data       [4]byte // __be32; invalidated_rkey when the flags say so
	Qp_num         uint32
	Src_qp         uint32
	Wc_flags       uint32
	Pkey_index     uint16
	Slid           uint16
	Sl             uint8
	Dlid_path_bits uint8
	_              [2]byte
}

// IbvMr is struct ibv_mr.
type IbvMr struct {
	Context uint64
	Pd      uint64
	Addr    uint64
	Length  uint64
	Handle  uint32
	Lkey    uint32
	Rkey    uint32
	_       [4]byte
}

// IbvCq is the leading part of struct ibv_cq, up to the mutex.
type IbvCq struct {
	Context    uint64
	Channel    uint64
	Cq_context uint64
	Handle     uint32
	Cqe        int32
}

// IbvQp is the leading part of struct ibv_qp, up to the mutex.
type IbvQp struct {
	Context    uint64
	Qp_context uint64
	Pd         uint64
	Send_cq    uint64
	Recv_cq    uint64
	Srq        uint64
	Handle     uint32
	Qp_num     uint32
	State      uint32
	Qp_type    uint32
}

// IbvQpCap is struct ibv_qp_cap.
type IbvQpCap struct {
	Max_send_wr     uint32
	Max_recv_wr     uint32
	Max_send_sge    uint32
	Max_recv_sge    uint32
	Max_inline_data uint32
}

// IbvQpInitAttr is struct ibv_qp_init_attr.
type IbvQpInitAttr struct {
	Qp_context uint64
	Send_cq    uint64
	Recv_cq    uint64
	Srq        uint64
	Cap        IbvQpCap
	Qp_type    uint32
	Sq_sig_all int32
	_          [4]byte
}

// IbvGlobalRoute is struct ibv_global_route.
type IbvGlobalRoute struct {
	Dgid          IbvGid
	Flow_label    uint32
	Sgid_index    uint8
	Hop_limit     uint8
	Traffic_class uint8
	_             [1]byte
}

// IbvAhAttr is struct ibv_ah_attr.
type IbvAhAttr struct {
	Grh           IbvGlobalRoute
	Dlid          uint16
	Sl            uint8
	Src_path_bits uint8
	Static_rate   uint8
	Is_global     uint8
	Port_num      uint8
	_             [1]byte
}

// IbvQpAttr is struct ibv_qp_attr.
type IbvQpAttr struct {
	Qp_state            uint32
	Cur_qp_state        uint32
	Path_mtu            uint32
	Path_mig_state      uint32
	Qkey                uint32
	Rq_psn              uint32
	Sq_psn              uint32
	Dest_qp_num         uint32
	Qp_access_flags     uint32
	Cap                 IbvQpCap
	Ah_attr             IbvAhAttr
	Alt_ah_attr         IbvAhAttr
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
	Rate_limit          uint32
	_                   [4]byte
}

// IbvSge is struct ibv_sge.
type IbvSge struct {
	Addr   uint64
	Length uint32
	Lkey   uint32
}

// IbvSendWr is struct ibv_send_wr. Of the wr union only the rdma member is
// named; the qp_type union and the bind_mw/tso union at the end are padding.
type IbvSendWr struct {
	Wr_id      uint64
	Next       uint64
	Sg_list    uint64
	Num_sge    int32
	Opcode     uint32
	Send_flags uint32
	Imm_data   [4]byte // __be32; invalidate_rkey for the *_INV opcodes
	Wr         struct {
		Rdma struct {
			Remote_addr uint64
			Rkey        uint32
			_           [4]byte
		}
		_ [16]byte
	}
	_ [56]byte
}

// IbvRecvWr is struct ibv_recv_wr.
type IbvRecvWr struct {
	Wr_id   uint64
	Next    uint64
	Sg_list uint64
	Num_sge int32
	_       [4]byte
}
