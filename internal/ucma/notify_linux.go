//go:build linux && !android && (amd64 || arm64 || loong64 || ppc64le || riscv64 || s390x)

package ucma

import "github.com/wzshiming/rdma/internal/rdmaabi"

// cmdNotify is the command of Notify, numbered as in rdma/rdma_cma_abi.h.
const cmdNotify = 15

// Notify reports an event of the queue pair of an identifier, an
// ibv_event_type value, to the kernel's connection manager, for a caller
// that manages the queue pair itself. On InfiniBand and RoCE the kernel
// takes IBV_EVENT_COMM_EST (4), which establishes a connection whose queue
// pair received before the connection was established, and
// IBV_EVENT_PATH_MIG (6); another event, or an identifier without a device
// or a connection, gets EINVAL. iWARP ignores the events.
func (c *Channel) Notify(id uint32, event uint32) error {
	cmd := rdmaabi.UcmaAbiNotify{Id: id, Event: event}
	return command(c, cmdNotify, &cmd, 0)
}
