//go:build linux && !android && (amd64 || arm64 || loong64 || ppc64le || riscv64 || s390x)

package cm_test

import (
	"context"
	"testing"

	"github.com/wzshiming/rdma/cm"
)

// TestDepthsSameAsReference compares the implementation under test with the
// reference on depths at the device's limits: connections whose depths are
// the 0xFF that stands for the limits on both ends, and a connect whose
// initiator depth exceeds the limit.
func TestDepthsSameAsReference(t *testing.T) {
	dev := testDevice(t)
	t.Run("the limits", func(t *testing.T) {
		limits := &cm.ConnParam{ResponderResources: 0xFF, InitiatorDepth: 0xFF}
		compare(t, dev, func(t *testing.T, im impl, at endpoints) connection {
			return connectAt(t, im, at, scenario{connect: limits, accept: limits})
		})
	})
	t.Run("above the limit", func(t *testing.T) { compare(t, dev, exceedAt) })
}

// exceedAt connects a client of im from at.client to a listener of im on
// at.listener with an initiator depth one above the device's limit.
func exceedAt(t *testing.T, im impl, at endpoints) failure {
	t.Helper()
	listenOn(t, im, at.listener)
	client := resolveFrom(t, im, at.client, at.listener)
	attr, err := client.Device().QueryDevice()
	if err != nil {
		t.Fatalf("QueryDevice() error: %v", err)
	}
	if attr.MaxQPInitRDAtom >= 0xFE {
		t.Skipf("the device's initiator depth limit %d leaves no depth above it", attr.MaxQPInitRDAtom)
	}
	ctx, cancel := context.WithTimeout(context.Background(), timeout)
	defer cancel()
	_, err = client.Connect(ctx, &cm.ConnParam{InitiatorDepth: uint8(attr.MaxQPInitRDAtom) + 1})
	if err == nil {
		t.Fatalf("Connect() with initiator depth %d succeeded, want it refused", attr.MaxQPInitRDAtom+1)
	}
	return failure{viewOf(err), qpState(t, client.QP())}
}
