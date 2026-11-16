package rsproto_test

import (
	"errors"
	"testing"

	"github.com/wzshiming/rdma/internal/rsproto"
)

// With a control slot free nothing is held: what is due goes out at once.
func TestControlHeldNotWithASlotFree(t *testing.T) {
	l := &recorder{}
	s := rsproto.NewStream(l, testConfig())
	s.Connect(testPeer)
	s.CloseWrite()

	if s.ControlHeld() {
		t.Errorf("ControlHeld() = true with the grant of Connect and RS_CTRL_SHUTDOWN in flight and slots free, want false")
	}
}

// Every slot in flight, with nothing due, holds nothing either.
func TestControlHeldNotWithNothingDue(t *testing.T) {
	c := testConfig()
	c.ReceiveQueueSize = rsproto.QPMinSize
	s := rsproto.NewStream(&recorder{}, c)
	s.Connect(testPeer)
	readMessages(s, c, 0, (rsproto.QPCtrlSize-1)*rsproto.QPMinSize/2)

	if s.ControlHeld() {
		t.Errorf("ControlHeld() = true with %d grants and credits in flight and none due, want false", rsproto.QPCtrlSize)
	}
}

// Credits due while every slot is in flight are held until a completion
// gives a slot back, when they go out.
func TestControlHeldWhileCreditsWaitForASlot(t *testing.T) {
	s, l := fourCreditsDue(t)
	if !s.ControlHeld() {
		t.Fatalf("ControlHeld() = false with %d grants and credits in flight and a fifth due, want true", rsproto.QPCtrlSize)
	}
	s.Sent(l.posts[0].id, nil)
	if s.ControlHeld() {
		t.Errorf("ControlHeld() = true once a completion let the credits due go out, want false")
	}
}

// A grant due while every slot is in flight is held as credits are.
func TestControlHeldWhileAGrantWaitsForASlot(t *testing.T) {
	c := testConfig()
	l := &recorder{}
	s := rsproto.NewStream(l, c)
	s.Connect(testPeer)
	half := len(c.ReceiveBuffer.Buf) / 2
	for i := range rsproto.QPCtrlSize {
		peerWrites(s, c, i%2*half, make([]byte, half))
		s.Peek()
		s.Consume(half)
	}
	if len(l.posts) != rsproto.QPCtrlSize || !s.ControlHeld() {
		t.Fatalf("reading %d halves of the receive buffer posted %d grants and ControlHeld() = %v, want %d and true", rsproto.QPCtrlSize, len(l.posts), s.ControlHeld(), rsproto.QPCtrlSize)
	}
	s.Sent(l.posts[0].id, nil)
	if s.ControlHeld() {
		t.Errorf("ControlHeld() = true once a completion let the grant due go out, want false")
	}
}

// Reading both halves of the receive buffer at once posts one grant, as
// rs_update_credits sends one a call; the second stays due, a slot free,
// until the completion of a control message brings the next call.
func TestControlHeldWhileASecondGrantIsDue(t *testing.T) {
	c := testConfig()
	l := &recorder{}
	s := rsproto.NewStream(l, c)
	s.Connect(testPeer)
	half := len(c.ReceiveBuffer.Buf) / 2
	peerWrites(s, c, 0, make([]byte, half))
	peerWrites(s, c, half, make([]byte, half))
	s.Peek()
	s.Consume(2 * half)
	if len(l.posts) != 2 || !s.ControlHeld() {
		t.Fatalf("reading both halves at once posted %d grants and ControlHeld() = %v, want 2 and true", len(l.posts), s.ControlHeld())
	}
	s.Sent(l.posts[0].id, nil)
	if len(l.posts) != 3 || s.ControlHeld() {
		t.Errorf("the completion of the grant of Connect posted %d grants in all and ControlHeld() = %v, want 3 and false", len(l.posts), s.ControlHeld())
	}
}

// An end CloseWrite or Disconnect holds for a slot is held as credits are.
func TestControlHeldWhileAnEndWaitsForASlot(t *testing.T) {
	for _, end := range []struct {
		name string
		f    func(*rsproto.Stream)
	}{
		{"CloseWrite", (*rsproto.Stream).CloseWrite},
		{"Disconnect", (*rsproto.Stream).Disconnect},
	} {
		t.Run(end.name, func(t *testing.T) {
			s, l := fourCreditsDue(t)
			s.Sent(l.posts[0].id, nil)
			end.f(s)
			if !s.ControlHeld() {
				t.Fatalf("ControlHeld() = false after %s with every slot in flight, want true", end.name)
			}
			s.Sent(l.posts[1].id, nil)
			if s.ControlHeld() {
				t.Errorf("ControlHeld() = true once a completion let the end of %s go out, want false", end.name)
			}
		})
	}
}

// Once the link is down nothing held goes out, so nothing is held.
func TestControlHeldNotOnceTheLinkIsDown(t *testing.T) {
	s, _ := fourCreditsDue(t)
	s.CloseWrite()
	s.LinkDown(errors.New("RDMA_CM_EVENT_DISCONNECTED"))

	if s.ControlHeld() {
		t.Errorf("ControlHeld() = true with credits due and an end held once the link is down, want false")
	}
}

// An end held as the peer's RS_CTRL_SHUTDOWN ends the connection still goes
// out, so it stays held.
func TestControlHeldWhileAnOwedEndWaitsForASlot(t *testing.T) {
	s, l := fourCreditsDue(t)
	s.Sent(l.posts[0].id, nil)
	s.CloseWrite()
	s.Received(rsproto.MakeMsg(rsproto.OpCtrl, rsproto.CtrlShutdown))
	if s.State()&rsproto.StateConnected != 0 || !s.ControlHeld() {
		t.Fatalf("after the peer's RS_CTRL_SHUTDOWN met the held end the state is %#x and ControlHeld() = %v, want the connection ended and true", s.State(), s.ControlHeld())
	}
	s.Sent(l.posts[1].id, nil)
	if s.ControlHeld() {
		t.Errorf("ControlHeld() = true once a completion let the owed end go out, want false")
	}
}
