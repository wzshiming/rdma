//go:build linux && !android && (amd64 || arm64 || loong64 || ppc64le || riscv64 || s390x)

package cm

import (
	"context"
	"net/netip"

	"github.com/wzshiming/rdma/internal/ucma"
	"github.com/wzshiming/rdma/verbs"
)

// datagram is the service of the datagram identifiers: UD queue pairs in the
// UDP port space, as udaddy and mckey create theirs.
var datagram = service{ucma.PortSpaceUDP, ucma.QPTypeUD}

// DatagramID is a communication identifier of the datagram service, with an
// event channel of its own: resolved to a peer by ResolveDatagram, bound to
// a local address by BindDatagram, or the identifier of a DatagramRequest.
type DatagramID struct {
	id *ID
}

// ResolveDatagram resolves the address and route to remote as Resolve does,
// for an identifier of the datagram service. It sets no option: it is
// Dialer{}.ResolveDatagram.
func ResolveDatagram(ctx context.Context, local, remote netip.AddrPort) (*DatagramID, error) {
	return Dialer{}.ResolveDatagram(ctx, local, remote)
}

// ResolveDatagram resolves the address and route to remote as the package's
// ResolveDatagram does, on an identifier with the options of d. The kernel
// refuses an ACK timeout for a datagram identifier, so a Dialer with one
// fails before any command with a *verbs.OpError of rdma_set_option that
// matches syscall.EINVAL.
func (d Dialer) ResolveDatagram(ctx context.Context, local, remote netip.AddrPort) (*DatagramID, error) {
	if err := checkResolveAddrs(local, remote); err != nil {
		return nil, err
	}
	opts := d.options()
	if err := opts.checkDatagram(); err != nil {
		return nil, err
	}
	if err := ctx.Err(); err != nil {
		return nil, err
	}
	if err := initialize("rdma_resolve_addr"); err != nil {
		return nil, err
	}
	id, err := config{datagram, opts}.resolve(ctx, local, remote)
	if err != nil {
		return nil, err
	}
	return &DatagramID{id}, nil
}

// BindDatagram binds an identifier of the datagram service to local, without
// a peer, as rdma_bind_addr does: the identifier for multicast. The IP of
// local picks the device, so it must be the address of one, not the
// unspecified address; port 0 picks a free port.
func BindDatagram(local netip.AddrPort) (*DatagramID, error) {
	if err := checkBindAddr(local); err != nil {
		return nil, err
	}
	if err := initialize("rdma_bind_addr"); err != nil {
		return nil, err
	}
	return config{datagram, options{}}.bind(local)
}

// bind creates an identifier with c and binds it to local, whose addresses,
// device and port it records as rdma_bind_addr does; a bind without a
// device is ENODEV.
func (c config) bind(local netip.AddrPort) (*DatagramID, error) {
	ch, handle, uid, err := c.create()
	if err != nil {
		return nil, err
	}
	id := newID(ch, handle, uid)
	id.svc = c.service
	err = ch.Bind(handle, local)
	if err == nil {
		err = id.queryAddr()
	}
	if err != nil {
		id.Close()
		return nil, opError("rdma_bind_addr", err)
	}
	return &DatagramID{id}, nil
}

// Device returns the device the identifier uses. The package keeps it open
// for the life of the process; Close on it releases nothing.
func (id *DatagramID) Device() *verbs.Context { return id.id.Device() }

// Port returns the device port the identifier uses.
func (id *DatagramID) Port() uint8 { return id.id.Port() }

// LocalAddr returns the local address of the identifier.
func (id *DatagramID) LocalAddr() netip.AddrPort { return id.id.LocalAddr() }

// RemoteAddr returns the address of the peer, the zero value for an
// identifier of BindDatagram.
func (id *DatagramID) RemoteAddr() netip.AddrPort { return id.id.RemoteAddr() }

// CreateQP creates the queue pair of the identifier (rdma_create_qp), whose
// attr.Type is verbs.QPTypeUD, and takes it to RTS as ucma_init_ud_qp does:
// INIT with the port, partition and Q_Key the kernel has for the
// identifier, RTR, and RTS with send PSN 0; no connection moves it later.
// The Q_Key is rdma_cm's for the UDP port space, 0x01234567. pd, attr and
// the life of the queue pair are as for ID.CreateQP.
func (id *DatagramID) CreateQP(pd *verbs.PD, attr *verbs.QPInitAttr) (*verbs.QP, error) {
	return id.id.CreateQP(pd, attr)
}

// QP returns the queue pair created by CreateQP, or nil.
func (id *DatagramID) QP() *verbs.QP { return id.id.QP() }

// qpModifier is a queue pair as initUD moves it; a test stands in for it.
type qpModifier interface {
	Modify(attr *verbs.QPAttr, mask verbs.QPAttrMask) error
}

// initUD takes a UD queue pair to RTS as ucma_init_ud_qp does: INIT with the
// attributes the kernel has for the identifier, its qkey among them, RTR
// with the state alone and RTS with send PSN 0.
func (id *ID) initUD(qp qpModifier) error {
	k, err := id.ch.InitQPAttr(id.handle, uint32(verbs.QPStateInit))
	if err != nil {
		return err
	}
	attr, mask := qpAttrFromKernel(verbs.QPStateInit, &k)
	if err := cause(qp.Modify(attr, mask)); err != nil {
		return err
	}
	if err := cause(qp.Modify(&verbs.QPAttr{State: verbs.QPStateRTR}, verbs.QPAttrState)); err != nil {
		return err
	}
	return cause(qp.Modify(&verbs.QPAttr{State: verbs.QPStateRTS}, verbs.QPAttrState|verbs.QPAttrSQPSN))
}

// Close destroys the queue pair and the identifier and wakes a pending wait
// with an error. It never blocks on pending events; a second Close is a
// no-op.
func (id *DatagramID) Close() error { return id.id.Close() }

// DatagramListener listens for the requests of the datagram service on an
// address.
type DatagramListener struct {
	l *Listener
}

// ListenDatagram binds local and listens for the requests of the datagram
// service as Listen does for connections. It sets no option: it is
// ListenConfig{}.ListenDatagram.
func ListenDatagram(local netip.AddrPort, backlog int) (*DatagramListener, error) {
	return ListenConfig{}.ListenDatagram(local, backlog)
}

// ListenDatagram binds local and listens as the package's ListenDatagram
// does, on an identifier with the options of lc; an ACK timeout fails it as
// it fails Dialer.ResolveDatagram.
func (lc ListenConfig) ListenDatagram(local netip.AddrPort, backlog int) (*DatagramListener, error) {
	if err := checkListenAddr(local); err != nil {
		return nil, err
	}
	opts := lc.options()
	if err := opts.checkDatagram(); err != nil {
		return nil, err
	}
	if err := initialize("rdma_listen"); err != nil {
		return nil, err
	}
	l, err := config{datagram, opts}.listen(local, backlog)
	if err != nil {
		return nil, err
	}
	return &DatagramListener{l}, nil
}

// Addr returns the bound address, with the port actually chosen for port 0.
func (l *DatagramListener) Addr() netip.AddrPort { return l.l.Addr() }

// Accept waits for the next request and returns it with its private data;
// the request's identifier is on an event channel of its own. It returns
// ctx.Err() when ctx ends first and an error wrapping net.ErrClosed once
// the DatagramListener is closed.
func (l *DatagramListener) Accept(ctx context.Context) (*DatagramRequest, error) {
	req, err := l.l.Accept(ctx)
	if err != nil {
		return nil, err
	}
	return &DatagramRequest{ID: &DatagramID{req.ID}, PrivateData: req.PrivateData}, nil
}

// Close stops listening and wakes a pending Accept with net.ErrClosed.
// Requests not yet accepted are rejected by the kernel. A second Close is a
// no-op.
func (l *DatagramListener) Close() error { return l.l.Close() }
