//go:build linux && !android && (amd64 || arm64 || loong64 || ppc64le || riscv64 || s390x)

package verbs_test

import (
	"errors"
	"strings"
	"syscall"
	"testing"

	"github.com/wzshiming/rdma/verbs"
)

// The software providers rxe and siw have no ECE: libibverbs answers both
// calls with EOPNOTSUPP, which matches errors.ErrUnsupported.
func TestProviderWithoutECEIsNotSupported(t *testing.T) {
	dev, ctx := openDevice(t)
	if !strings.HasPrefix(dev.Name, "rxe") && !strings.HasPrefix(dev.Name, "siw") {
		t.Skipf("%s is not rxe or siw, whose providers have no ECE", dev.Name)
	}
	qp, _ := createRC(t, allocPD(t, ctx), createCQ(t, ctx, nil))
	ece, err := qp.QueryECE()
	var opErr *verbs.OpError
	if ece != (verbs.ECE{}) || !errors.As(err, &opErr) || opErr.Op != "ibv_query_ece" || !errors.Is(err, syscall.EOPNOTSUPP) {
		t.Errorf("QueryECE() = %+v, %v; want none and an *OpError of ibv_query_ece with EOPNOTSUPP", ece, err)
	}
	if !errors.Is(err, errors.ErrUnsupported) {
		t.Errorf("QueryECE() error = %v, want one matching errors.ErrUnsupported", err)
	}
	if err := qp.SetECE(verbs.ECE{VendorID: 0x02c9, Options: 1}); !errors.As(err, &opErr) || opErr.Op != "ibv_set_ece" || !errors.Is(err, syscall.EOPNOTSUPP) {
		t.Errorf("SetECE() error = %v, want an *OpError of ibv_set_ece with EOPNOTSUPP", err)
	}
}
