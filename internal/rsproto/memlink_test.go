package rsproto_test

import (
	"bytes"
	"testing"

	"github.com/wzshiming/rdma/internal/rsproto"
)

// An RDMA write with immediate data gathers its local segments into the
// memory the peer registered under the key it names, at the address it
// names, and its message word arrives with it.
func TestMemLinkWriteLandsAtAddressAndKey(t *testing.T) {
	a, b := rsproto.NewMemLink()
	src := rsproto.Region{Buf: []byte("hel--lo"), Addr: 0x1000, Key: 1}
	other := rsproto.Region{Buf: make([]byte, 16), Addr: 0x2000, Key: 2}
	dst := rsproto.Region{Buf: make([]byte, 16), Addr: 0x2000, Key: 3}
	a.Register(src)
	b.Register(other)
	b.Register(dst)
	if err := b.PostRecv(1 << 63); err != nil {
		t.Fatalf("PostRecv = %v", err)
	}

	sgl := []rsproto.Segment{{Addr: 0x1000, Length: 3, Key: 1}, {Addr: 0x1005, Length: 2, Key: 1}}
	msg := rsproto.MakeMsg(rsproto.OpData, 5)
	if err := a.PostWriteMsg(7, sgl, msg, 0x2004, 3); err != nil {
		t.Fatalf("PostWriteMsg = %v", err)
	}
	if got, ok := b.Deliver(); !ok || got != msg {
		t.Fatalf("Deliver() = %#x, %v, want the message word %#x", got, ok, msg)
	}
	if want := []byte("\x00\x00\x00\x00hello\x00\x00\x00\x00\x00\x00\x00"); !bytes.Equal(dst.Buf, want) || !bytes.Equal(other.Buf, make([]byte, 16)) {
		t.Errorf("the write of %q to 0x2004 under key 3 left key 3 holding %q and key 2 %q, want %q and zeros", "hello", dst.Buf, other.Buf, want)
	}
}

// A message word alone, an RDMA write with immediate data of no bytes,
// arrives at the peer, once, and not back at its sender.
func TestMemLinkMessageAloneArrivesAtThePeer(t *testing.T) {
	a, b := rsproto.NewMemLink()
	if err := b.PostRecv(1 << 63); err != nil {
		t.Fatalf("PostRecv = %v", err)
	}
	msg := rsproto.MakeMsg(rsproto.OpCtrl, rsproto.CtrlShutdown)
	if err := a.PostMsg(9, msg); err != nil {
		t.Fatalf("PostMsg = %v", err)
	}

	if got, ok := a.Deliver(); ok {
		t.Fatalf("the sender's own Deliver() = %#x, want nothing", got)
	}
	if got, ok := b.Deliver(); !ok || got != msg {
		t.Fatalf("Deliver() = %#x, %v, want the message word %#x", got, ok, msg)
	}
	if got, ok := b.Deliver(); ok {
		t.Errorf("a second Deliver() = %#x, want nothing", got)
	}
}
