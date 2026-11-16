//go:build linux && !android && (amd64 || arm64 || loong64 || ppc64le || riscv64 || s390x)

package ucma

import (
	"bytes"
	"context"
	"errors"
	"os"
	"runtime"
	"strconv"
	"syscall"
	"time"
	"unsafe"

	"github.com/wzshiming/rdma/internal/rdmaabi"
)

// EventType is the type of an event (enum rdma_cm_event_type).
type EventType uint32

// Event types.
const (
	EventAddrResolved EventType = iota
	EventAddrError
	EventRouteResolved
	EventRouteError
	EventConnectRequest
	EventConnectResponse
	EventConnectError
	EventUnreachable
	EventRejected
	EventEstablished
	EventDisconnected
	EventDeviceRemoval
	EventMulticastJoin
	EventMulticastError
	EventAddrChange
	EventTimewaitExit
)

var eventNames = [...]string{
	"ADDR_RESOLVED", "ADDR_ERROR", "ROUTE_RESOLVED", "ROUTE_ERROR", "CONNECT_REQUEST", "CONNECT_RESPONSE",
	"CONNECT_ERROR", "UNREACHABLE", "REJECTED", "ESTABLISHED", "DISCONNECTED", "DEVICE_REMOVAL",
	"MULTICAST_JOIN", "MULTICAST_ERROR", "ADDR_CHANGE", "TIMEWAIT_EXIT",
}

func (t EventType) String() string {
	if int(t) < len(eventNames) {
		return eventNames[t]
	}
	return "EventType(" + strconv.FormatUint(uint64(t), 10) + ")"
}

// Event is an event of an identifier (struct ucma_abi_event_resp).
type Event struct {
	Type EventType
	// Status is 0, a negative errno, or for EventRejected the reason.
	Status int32
	// UID and ID are the identifier's; for EventConnectRequest, UID is the
	// listener's and ID the new identifier of the request.
	UID uint64
	ID  uint32
	ECE ECE
	// param is the union of the two parameter forms, as long as the
	// datagram form.
	param rdmaabi.UcmaAbiUdParam
}

// Conn reads the parameters in the connection form, which the kernel fills
// for an identifier whose QP type is not UD.
func (e Event) Conn() ConnParam {
	p := (*rdmaabi.UcmaAbiConnParam)(unsafe.Pointer(&e.param))
	return ConnParam{
		PrivateData:        privateDataOf(p.Private_data[:p.Private_data_len]),
		QPNum:              p.Qp_num,
		SRQ:                p.Srq != 0,
		ResponderResources: p.Responder_resources,
		InitiatorDepth:     p.Initiator_depth,
		FlowControl:        p.Flow_control,
		RetryCount:         p.Retry_count,
		RNRRetryCount:      p.Rnr_retry_count,
	}
}

// UDParam is the datagram form of the parameters of an event (struct
// ucma_abi_ud_param).
type UDParam struct {
	PrivateData []byte
	QPNum       uint32
	QKey        uint32
	AHAttr      rdmaabi.IbUverbsAhAttr
}

// UD reads the parameters in the datagram form, which the kernel fills for
// an identifier whose QP type is UD.
func (e Event) UD() UDParam {
	p := &e.param
	return UDParam{
		PrivateData: privateDataOf(p.Private_data[:p.Private_data_len]),
		QPNum:       p.Qp_num,
		QKey:        p.Qkey,
		AHAttr:      p.Ah_attr,
	}
}

// privateDataOf copies the private data of an event; none gives nil.
func privateDataOf(b []byte) []byte {
	if len(b) == 0 {
		return nil
	}
	return bytes.Clone(b)
}

func eventOf(r *rdmaabi.UcmaAbiEventResp) Event {
	return Event{
		Type:   EventType(r.Event),
		Status: int32(r.Status),
		UID:    r.Uid,
		ID:     r.Id,
		ECE:    ECE{VendorID: r.Ece.Vendor_id, AttrMod: r.Ece.Attr_mod},
		param:  *(*rdmaabi.UcmaAbiUdParam)(unsafe.Pointer(&r.Param)),
	}
}

// aLongTimeAgo is a read deadline that is already over.
var aLongTimeAgo = time.Unix(1, 0)

// GetEvent waits for the next event of the channel and fetches it. The wait
// is the runtime poller's and holds no OS thread. It returns ctx.Err() when
// ctx ends first and os.ErrClosed once the channel is closed. Waiters take
// turns: one waits on the descriptor while the others queue, each bound by
// its own context.
func (c *Channel) GetEvent(ctx context.Context) (Event, error) {
	if err := ctx.Err(); err != nil {
		return Event{}, err
	}
	select {
	case c.turn <- struct{}{}:
	case <-ctx.Done():
		return Event{}, ctx.Err()
	}
	defer func() { <-c.turn }()
	stop := context.AfterFunc(ctx, func() { c.file.SetReadDeadline(aLongTimeAgo) })
	defer stop()

	var resp rdmaabi.UcmaAbiEventResp
	var pin runtime.Pinner
	defer pin.Unpin()
	cmd := rdmaabi.UcmaAbiGetEvent{Response: pinned(&pin, &resp)}
	b := request(cmdGetEvent, &cmd, unsafe.Sizeof(resp))
	for {
		var n int
		var errno syscall.Errno
		err := c.raw.Read(func(fd uintptr) bool {
			n, errno = c.write(fd, b)
			return errno != syscall.EAGAIN
		})
		if err == nil {
			if err := writeResult(n, errno, len(b)); err != nil {
				return Event{}, err
			}
			return eventOf(&resp), nil
		}
		if !errors.Is(err, os.ErrDeadlineExceeded) {
			return Event{}, os.ErrClosed
		}
		// The deadline is ctx's, or one that an earlier wait's context set
		// as that wait returned.
		c.file.SetReadDeadline(time.Time{})
		if err := ctx.Err(); err != nil {
			return Event{}, err
		}
	}
}
