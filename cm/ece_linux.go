//go:build linux && !android && (amd64 || arm64 || loong64 || ppc64le || riscv64 || s390x)

package cm

import (
	"errors"
	"syscall"

	"github.com/wzshiming/rdma/internal/ucma"
	"github.com/wzshiming/rdma/verbs"
)

// eceQP is what the vendor option exchange (ECE) uses of a queue pair; a
// *verbs.QP is one.
type eceQP interface {
	QueryECE() (verbs.ECE, error)
	SetECE(verbs.ECE) error
}

// eceOf is the ECE side of qp, a variable so that tests can give it ECE.
var eceOf = func(qp *verbs.QP) eceQP { return qp }

// eceError is the error an ECE call of verbs fails a step with: its errno,
// and none for EOPNOTSUPP, with which a provider or a libibverbs without ECE
// takes part in no exchange.
func eceError(err error) error {
	if errors.Is(err, syscall.EOPNOTSUPP) {
		return nil
	}
	return cause(err)
}

// receivedECE keeps the ECE the peer sent as its own, as
// ucma_process_conn_req does for a request and rdma_get_cm_event for a
// connect response that finds no queue pair.
func (id *ID) receivedECE(peer ucma.ECE) {
	id.remoteECE = verbs.ECE{VendorID: peer.VendorID, Options: peer.AttrMod}
}

// initECE takes the ECE of a new queue pair as the local one, as init_ece
// does; on the queue pair of a request it then sets the request's ECE when
// the vendors match, and the local vendor without options when they do not.
func (id *ID) initECE(qp eceQP) error {
	local, err := qp.QueryECE()
	if err := eceError(err); err != nil {
		return err
	}
	id.localECE = local
	if id.remoteECE.VendorID == 0 {
		return nil
	}
	set := verbs.ECE{VendorID: id.localECE.VendorID}
	if id.remoteECE.VendorID == id.localECE.VendorID {
		set = id.remoteECE
	}
	return eceError(qp.SetECE(set))
}

// setLocalECE takes the options the queue pair of a request accepted as the
// local ones, as set_local_ece does once the queue pair is in INIT.
func (id *ID) setLocalECE(qp eceQP) error {
	if id.remoteECE.VendorID == 0 {
		return nil
	}
	return id.queryLocalECE(qp)
}

// queryLocalECE takes the options qp accepted as the local ones; a queue
// pair without ECE has none.
func (id *ID) queryLocalECE(qp eceQP) error {
	got, err := qp.QueryECE()
	if err := eceError(err); err != nil {
		return err
	}
	id.localECE.Options = got.Options
	return nil
}

// connectECE is the ECE of CONNECT as ucma_copy_ece_param_to_kern_req writes
// it: the local one.
func (id *ID) connectECE() ucma.ECE {
	return ucma.ECE{VendorID: id.localECE.VendorID, AttrMod: id.localECE.Options}
}

// acceptECE is the ECE of ACCEPT as ucma_copy_ece_param_to_kern_rep writes
// it: the peer's vendor, returned as received, with the local options.
func (id *ID) acceptECE() ucma.ECE {
	return ucma.ECE{VendorID: id.remoteECE.VendorID, AttrMod: id.localECE.Options}
}

// answerECE is the ECE ucma_process_conn_resp_ece sets on the queue pair for
// the peer's answer to CONNECT: the answer, or the local vendor without
// options when the peer answered without ECE. An answer of another vendor
// than the local one is EINVAL.
func (id *ID) answerECE(answer ucma.ECE) (verbs.ECE, error) {
	set := verbs.ECE{VendorID: answer.VendorID, Options: answer.AttrMod}
	if answer.VendorID == 0 {
		set = verbs.ECE{VendorID: id.localECE.VendorID}
	} else if answer.VendorID != id.localECE.VendorID {
		return verbs.ECE{}, syscall.EINVAL
	}
	id.remoteECE.VendorID = answer.VendorID
	return set, nil
}
