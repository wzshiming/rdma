package rdma

import (
	"fmt"
	"math"
	"strconv"
	"strings"
	"testing"
)

// rstreamOp is what an op of a benchmark row is in rstream's custom test,
// whose client ends its statistics line with usec/xfer, the time of a
// transfer one way.
type rstreamOp int

const (
	noRstream        rstreamOp = iota // the row has no counterpart in rstream
	rstreamRoundTrip                  // a transfer each way
	rstreamStream                     // a transfer one way
)

// xfers returns the transfers of op.
func (op rstreamOp) xfers() int {
	if op == rstreamRoundTrip {
		return 2
	}
	return 1
}

// rstreamPerOp reads the statistics line of out, what rstream's client
// printed, and returns the time and the throughput of an op of op of size
// bytes a transfer.
func rstreamPerOp(out string, op rstreamOp, size int) (nsPerOp, mbPerSec float64, err error) {
	for line := range strings.Lines(out) {
		fields := strings.Fields(line)
		if len(fields) == 0 || fields[0] != "custom" {
			continue
		}
		usec, err := strconv.ParseFloat(fields[len(fields)-1], 64)
		if err != nil {
			return 0, 0, fmt.Errorf("rstream's statistics line %q: %v", line, err)
		}
		nsPerOp = usec * 1e3 * float64(op.xfers())
		return nsPerOp, float64(op.xfers()*size) / nsPerOp * 1e3, nil
	}
	return 0, 0, fmt.Errorf("rstream printed no statistics line: %q", out)
}

// rstreamOutput is what rstream's client printed in a run of the interop
// tests on rxe, 10 iterations of 10 transfers of 1000 bytes.
const rstreamOutput = "name      bytes   xfers   iters   total       time     Gb/sec    usec/xfer\n" +
	"custom    1000    10      10      195k        0.00s      0.40      20.05\n"

// A round trip takes two of the transfers whose time rstream prints, a
// transfer of a stream one, and the throughput is what rstream's Gb/sec
// rounds.
func TestRstreamPerOp(t *testing.T) {
	const size, usec, gbps = 1000, 20.05, 0.40
	for _, tt := range []struct {
		name   string
		op     rstreamOp
		wantNs float64
	}{
		{"round trip", rstreamRoundTrip, 2 * usec * 1e3},
		{"stream", rstreamStream, usec * 1e3},
	} {
		t.Run(tt.name, func(t *testing.T) {
			ns, mbps, err := rstreamPerOp(rstreamOutput, tt.op, size)
			if err != nil {
				t.Fatalf("rstreamPerOp error: %v", err)
			}
			if math.Abs(ns-tt.wantNs) > 1e-9*tt.wantNs {
				t.Errorf("rstreamPerOp = %v ns/op, want %v", ns, tt.wantNs)
			}
			if want := gbps * 1e3 / 8; math.Abs(mbps-want) > 0.005*1e3/8 {
				t.Errorf("rstreamPerOp = %v MB/s, want %v, rstream's %v Gb/sec, within its rounding", mbps, want, gbps)
			}
		})
	}
	t.Run("no statistics line", func(t *testing.T) {
		if _, _, err := rstreamPerOp("name      bytes   xfers   iters   total       time     Gb/sec    usec/xfer\n", rstreamStream, size); err == nil {
			t.Error("rstreamPerOp of output without a statistics line succeeded")
		}
	})
}
