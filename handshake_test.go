package rdma

import (
	"errors"
	"strings"
	"testing"

	"github.com/wzshiming/rdma/cm"
)

func TestHelloRoundTripsThroughPaddedPrivateData(t *testing.T) {
	cfg := Config{MessageSize: 4096, ReceiveDepth: 7, SendDepth: 3}
	data := encodeHello(cfg)
	if len(data) > cm.MaxConnectPrivateData {
		t.Fatalf("hello is %d bytes, the connect request carries at most %d", len(data), cm.MaxConnectPrivateData)
	}
	// The peer sees the field zero padded.
	padded := make([]byte, cm.MaxConnectPrivateData)
	copy(padded, data)
	h, err := decodeHello(padded)
	if err != nil {
		t.Fatalf("decodeHello() error: %v", err)
	}
	if h.version != protocolVersion || h.messageSize != 4096 || h.receiveDepth != 7 {
		t.Errorf("decodeHello() = %+v, want version %d, 4096, 7", h, protocolVersion)
	}
	reply := make([]byte, cm.MaxAcceptPrivateData)
	copy(reply, encodeHello(cfg))
	if h2, err := decodeHello(reply); err != nil || h2 != h {
		t.Errorf("decodeHello(reply) = %+v, %v; want %+v", h2, err, h)
	}
}

func TestDecodeHelloRejectsForeignData(t *testing.T) {
	for _, b := range [][]byte{nil, make([]byte, 8), []byte("hello world, this is not an rdma stream"), append([]byte("rdmX"), make([]byte, 12)...)} {
		if _, err := decodeHello(b); err == nil {
			t.Errorf("decodeHello(%q) = nil error, want a protocol error", b)
		} else if !errors.Is(err, ErrProtocol) {
			t.Errorf("decodeHello(%q) error = %v, want ErrProtocol", b, err)
		}
	}
}

func TestNegotiateTakesTheMinimum(t *testing.T) {
	own := Config{MessageSize: 16384, ReceiveDepth: 32, SendDepth: 8, MaxInline: 64}
	got, err := negotiate(own, hello{version: protocolVersion, messageSize: 4096, receiveDepth: 64})
	if err != nil {
		t.Fatalf("negotiate() error: %v", err)
	}
	want := Config{MessageSize: 4096, ReceiveDepth: 32, SendDepth: 8, MaxInline: 64}
	if got != want {
		t.Errorf("negotiate() = %+v, want %+v", got, want)
	}
}

func TestNegotiateRejectsVersionAndNonsense(t *testing.T) {
	own := Config{}.withDefaults()
	tests := []struct {
		name string
		peer hello
		code uint8
	}{
		{"future version", hello{version: protocolVersion + 1, messageSize: 4096, receiveDepth: 1}, rejectVersion},
		{"version zero", hello{version: 0, messageSize: 4096, receiveDepth: 1}, rejectVersion},
		{"tiny message", hello{version: protocolVersion, messageSize: 16, receiveDepth: 1}, rejectMalformed},
		{"no depth", hello{version: protocolVersion, messageSize: 4096, receiveDepth: 0}, rejectMalformed},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			_, err := negotiate(own, tt.peer)
			var rej *rejection
			if !errors.As(err, &rej) || rej.code != tt.code {
				t.Fatalf("negotiate() error = %v, want a rejection with code %d", err, tt.code)
			}
			if !errors.Is(err, ErrProtocol) {
				t.Errorf("errors.Is(err, ErrProtocol) = false")
			}
		})
	}
}

func TestRejectFrameExplainsItselfToTheDialer(t *testing.T) {
	frame := encodeReject(rejectVersion)
	if len(frame) > cm.MaxRejectPrivateData {
		t.Fatalf("reject frame is %d bytes, the reject carries at most %d", len(frame), cm.MaxRejectPrivateData)
	}
	padded := make([]byte, cm.MaxRejectPrivateData)
	copy(padded, frame)
	err := rejectedError(&cm.RejectedError{PrivateData: padded, Reason: 28})
	if !errors.Is(err, ErrProtocol) {
		t.Fatalf("rejectedError() = %v, want ErrProtocol", err)
	}
	if !strings.Contains(err.Error(), "version") {
		t.Errorf("rejectedError() = %q, want it to name the version mismatch", err)
	}
	// A reject that is not ours stays what cm reported.
	foreign := &cm.RejectedError{PrivateData: []byte("go away"), Reason: 28}
	if err := rejectedError(foreign); err != foreign {
		t.Errorf("rejectedError(foreign) = %v, want the error unchanged", err)
	}
}
