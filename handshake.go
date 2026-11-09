package rdma

import (
	"encoding/binary"
	"errors"
	"fmt"

	"github.com/wzshiming/rdma/cm"
)

// ErrProtocol is matched by errors.Is for errors caused by a peer that does
// not speak this package's protocol: a handshake rejected for a version
// mismatch, or a malformed message on an established connection.
var ErrProtocol = errors.New("protocol error")

// protocolVersion is the version of the wire protocol; peers with a different
// version are rejected during the handshake.
const protocolVersion = 1

// handshakeMagic opens every private data frame so that zero padding and
// foreign peers are told apart from our own frames.
var handshakeMagic = [4]byte{'r', 'd', 'm', 's'}

// helloSize is the size of a hello frame, the dialer's proposal and the
// listener's reply:
//
//	0  magic "rdms"
//	4  protocol version
//	5  reserved (3 bytes)
//	8  message size (uint32, big endian)
//	12 receive depth (uint32, big endian)
const helloSize = 16

// hello is the decoded form of a hello frame.
type hello struct {
	version      uint8
	messageSize  uint32
	receiveDepth uint32
}

// encodeHello frames the fields of c that the peer needs.
func encodeHello(c Config) []byte {
	b := make([]byte, helloSize)
	copy(b, handshakeMagic[:])
	b[4] = protocolVersion
	binary.BigEndian.PutUint32(b[8:], uint32(c.MessageSize))
	binary.BigEndian.PutUint32(b[12:], uint32(c.ReceiveDepth))
	return b
}

// decodeHello parses a hello frame, which may carry trailing zero padding.
func decodeHello(b []byte) (hello, error) {
	if len(b) < helloSize || [4]byte(b[:4]) != handshakeMagic {
		return hello{}, fmt.Errorf("%w: peer did not send an rdma stream handshake", ErrProtocol)
	}
	return hello{
		version:      b[4],
		messageSize:  binary.BigEndian.Uint32(b[8:]),
		receiveDepth: binary.BigEndian.Uint32(b[12:]),
	}, nil
}

// Reasons a listener rejects a handshake with.
const (
	rejectVersion   uint8 = 1 // the peer speaks another protocol version
	rejectMalformed uint8 = 2 // the peer's proposal makes no sense
	rejectResources uint8 = 3 // the listener could not set the connection up
)

// rejection is the error negotiate returns for a proposal the listener must
// reject; code goes into the reject frame.
type rejection struct {
	code uint8
	msg  string
}

func (r *rejection) Error() string { return "protocol error: " + r.msg }

// Is reports whether target is ErrProtocol.
func (r *rejection) Is(target error) bool { return target == ErrProtocol }

// negotiate combines the listener's configuration with the dialer's proposal:
// message size and receive depth take the smaller value of the two.
func negotiate(own Config, peer hello) (Config, error) {
	if peer.version != protocolVersion {
		return Config{}, &rejection{rejectVersion, fmt.Sprintf("peer speaks protocol version %d, want %d", peer.version, protocolVersion)}
	}
	if peer.messageSize < minMessageSize || peer.messageSize > maxMessageSize || peer.receiveDepth < 1 || peer.receiveDepth > maxDepth {
		return Config{}, &rejection{rejectMalformed, fmt.Sprintf("peer proposed message size %d and receive depth %d", peer.messageSize, peer.receiveDepth)}
	}
	own.MessageSize = min(own.MessageSize, int(peer.messageSize))
	own.ReceiveDepth = min(own.ReceiveDepth, int(peer.receiveDepth))
	return own, nil
}

// rejectSize is the size of a reject frame:
//
//	0 magic "rdms"
//	4 protocol version of the rejecting side
//	5 reason code
const rejectSize = 6

// encodeReject frames the reason a listener rejects a request with.
func encodeReject(code uint8) []byte {
	b := make([]byte, rejectSize)
	copy(b, handshakeMagic[:])
	b[4] = protocolVersion
	b[5] = code
	return b
}

// rejectedError translates a rejection by the peer: one carrying our reject
// frame becomes an error matching ErrProtocol, anything else is returned as
// it is.
func rejectedError(rej *cm.RejectedError) error {
	b := rej.PrivateData
	if len(b) < rejectSize || [4]byte(b[:4]) != handshakeMagic {
		return rej
	}
	switch b[5] {
	case rejectVersion:
		return fmt.Errorf("%w: handshake rejected, peer speaks protocol version %d, want %d", ErrProtocol, b[4], protocolVersion)
	case rejectMalformed:
		return fmt.Errorf("%w: handshake rejected as malformed by the peer", ErrProtocol)
	case rejectResources:
		return fmt.Errorf("%w: handshake rejected, peer could not set the connection up", ErrProtocol)
	}
	return fmt.Errorf("%w: handshake rejected with reason %d", ErrProtocol, b[5])
}
