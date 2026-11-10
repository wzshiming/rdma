//go:build !linux || android || !(amd64 || arm64 || loong64 || ppc64le || riscv64 || s390x)

package verbs_test

import (
	"context"
	"errors"
	"testing"

	"github.com/wzshiming/rdma/verbs"
)

func TestStubUnsupported(t *testing.T) {
	ctx := verbs.ContextFromPointer(nil)
	pd := verbs.PDFromPointer(nil)
	cq := verbs.CQFromPointer(nil)
	ch := verbs.CompChannelFromPointer(nil)
	qp := verbs.QPFromPointer(nil)
	tests := []struct {
		name string
		call func() error
	}{
		{"Devices", func() error { _, err := verbs.Devices(); return err }},
		{"ForkInit", verbs.ForkInit},
		{"Context.QueryDevice", func() error { _, err := ctx.QueryDevice(); return err }},
		{"Context.QueryPort", func() error { _, err := ctx.QueryPort(1); return err }},
		{"Context.GIDTable", func() error { _, err := ctx.GIDTable(1); return err }},
		{"Context.AllocPD", func() error { _, err := ctx.AllocPD(); return err }},
		{"PD.RegisterMemory", func() error { _, err := pd.RegisterMemory(make([]byte, 16), verbs.AccessLocalWrite); return err }},
		{"PD.AllocMemory", func() error { _, err := pd.AllocMemory(4096, verbs.AccessLocalWrite); return err }},
		{"Context.CreateCompChannel", func() error { _, err := ctx.CreateCompChannel(); return err }},
		{"Context.CreateCQ", func() error { _, err := ctx.CreateCQ(16, nil); return err }},
		{"Context.WaitAsyncEvent", func() error { _, err := ctx.WaitAsyncEvent(context.Background()); return err }},
		{"CQ.Poll", func() error { _, err := cq.Poll(make([]verbs.WC, 1)); return err }},
		{"CQ.RequestNotify", func() error { return cq.RequestNotify(false) }},
		{"CompChannel.Wait", func() error { _, err := ch.Wait(context.Background()); return err }},
		{"PD.CreateQP", func() error { _, err := pd.CreateQP(&verbs.QPInitAttr{Type: verbs.QPTypeRC}); return err }},
		{"QP.Modify", func() error { return qp.Modify(&verbs.QPAttr{State: verbs.QPStateInit}, verbs.QPAttrState) }},
		{"QP.Query", func() error { _, err := qp.Query(verbs.QPAttrState); return err }},
		{"QP.PostSend", func() error { return qp.PostSend(verbs.SendWR{Op: verbs.WRSend}) }},
		{"QP.PostRecv", func() error { return qp.PostRecv(verbs.RecvWR{}) }},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			err := tt.call()
			if !errors.Is(err, errors.ErrUnsupported) {
				t.Fatalf("err = %v, want errors.ErrUnsupported", err)
			}
			var opErr *verbs.OpError
			if !errors.As(err, &opErr) {
				t.Fatalf("err = %T, want *verbs.OpError", err)
			}
		})
	}
	if ctx.Pointer() != nil {
		t.Errorf("Pointer() = %v, want nil", ctx.Pointer())
	}
	if err := ctx.Close(); err != nil {
		t.Errorf("Close() = %v, want nil", err)
	}
	if err := pd.Close(); err != nil {
		t.Errorf("PD.Close() = %v, want nil", err)
	}
	mr := verbs.MRFromPointer(nil)
	if b := mr.Bytes(); b != nil {
		t.Errorf("MR.Bytes() = %v, want nil", b)
	}
	if err := mr.Close(); err != nil {
		t.Errorf("MR.Close() = %v, want nil", err)
	}
	for name, closer := range map[string]interface{ Close() error }{"CQ": cq, "CompChannel": ch, "QP": qp} {
		if err := closer.Close(); err != nil {
			t.Errorf("%s.Close() = %v, want nil", name, err)
		}
	}
	if qp.Num() != 0 || qp.Pointer() != nil || cq.Pointer() != nil || ch.Pointer() != nil {
		t.Error("stub handles are not zero")
	}
}
