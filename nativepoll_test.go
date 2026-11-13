package rdma

import (
	"errors"
	"io"
	"net"
	"os"
	"runtime"
	"strings"
	"sync/atomic"
	"testing"
	"time"
)

// testPollTime is the PollTime the tests of the poll switch polling on with,
// the polling_time of rsockets.
const testPollTime = 10 * time.Microsecond

// pollClock is the clock the waits of a stream under test poll by, which
// stands still until the test moves it on.
type pollClock struct{ at atomic.Int64 }

func (c *pollClock) now() time.Duration {
	// A poll spins on the clock: the test and the device get a turn.
	runtime.Gosched()
	return time.Duration(c.at.Load())
}

func (c *pollClock) advance(d time.Duration) { c.at.Add(int64(d)) }

// stopPollClock has the waits of c, a connection of the memory engine, poll
// by a clock that stands still until the test moves it on, if they poll.
func stopPollClock(c *Conn) *pollClock {
	clk := new(pollClock)
	if p := c.s.(*nativeStream).poller; p != nil {
		p.now = clk.now
	}
	return clk
}

// pollRead starts a Read of one byte from c.
func pollRead(c *Conn) <-chan copied {
	return async(func() (int64, error) {
		n, err := c.Read(make([]byte, 1))
		return int64(n), err
	})
}

// awaitPolling waits until a wait on tr polls: it has had tr process again
// and again since tr counted from.
func awaitPolling(t *testing.T, tr *memTransport, from int64) {
	t.Helper()
	eventually(t, "the wait polling", func() bool { return tr.processCount() >= from+3 })
}

// A wait whose poll finds the stream ready returns without declaring what it
// waits for, so the queues are never armed: nothing processes in the
// background, and a Read that parked would never get the message.
func TestNativePollFindsWhatComesWithinThePollTime(t *testing.T) {
	client, server := nativeOnMemLink.pair(t, Config{PollTime: testPollTime})
	stopPollClock(server)
	tr := memTransportOf(server)
	processed, expects, arms := tr.processCount(), tr.expectCount(arrivals), tr.armCount()
	read := pollRead(server)
	awaitPolling(t, tr, processed)
	if _, err := client.Write([]byte("x")); err != nil {
		t.Fatalf("Write error: %v", err)
	}
	if r := await(t, "the polling Read", read); r.n != 1 || r.err != nil {
		t.Fatalf("the polling Read = %d, %v; want 1 and nil", r.n, r.err)
	}
	if n := tr.expectCount(arrivals) - expects; n != 0 {
		t.Errorf("the Read whose poll found the message declared %d times that it waits for arrivals, want never", n)
	}
	if n := tr.armCount() - arms; n != 0 {
		t.Errorf("the queues were armed %d times for the Read whose poll found the message, want never", n)
	}
}

// A poll that finds nothing until the poll time has passed leaves the wait
// to declare, once, that it waits for arrivals and to park: it processes no
// more, and the message that comes then reaches it in the background.
func TestNativePollThatFindsNothingDeclaresOnceAndParks(t *testing.T) {
	client, server := nativeOnMemLink.pair(t, Config{PollTime: testPollTime})
	clk := stopPollClock(server)
	tr := memTransportOf(server)
	processed, expects := tr.processCount(), tr.expectCount(arrivals)
	read := pollRead(server)
	awaitPolling(t, tr, processed)
	clk.advance(testPollTime)
	eventually(t, "the Read declared that it waits for arrivals", func() bool { return tr.expectCount(arrivals) > expects })
	parked := tr.processCount()
	if _, err := client.Write([]byte("x")); err != nil {
		t.Fatalf("Write error: %v", err)
	}
	if r := await(t, "the parked Read", read); r.n != 1 || r.err != nil {
		t.Fatalf("the parked Read = %d, %v; want 1 and nil", r.n, r.err)
	}
	if n := tr.expectCount(arrivals) - expects; n != 1 {
		t.Errorf("the Read whose poll found nothing declared %d times that it waits for arrivals, want once", n)
	}
	if n := tr.processCount() - parked; n != 0 {
		t.Errorf("the Read had the transport process %d times after it declared, want it parked", n)
	}
}

// A deadline that passes and Close end a poll while its clock stands still.
func TestNativePollEndsAtTheDeadlineAndAtClose(t *testing.T) {
	for _, tt := range []struct {
		name string
		end  func(*Conn) error
		want error
	}{
		{"deadline", func(c *Conn) error { return c.SetReadDeadline(time.Now()) }, os.ErrDeadlineExceeded},
		{"close", (*Conn).Close, net.ErrClosed},
	} {
		t.Run(tt.name, func(t *testing.T) {
			_, server := nativeOnMemLink.pair(t, Config{PollTime: testPollTime})
			stopPollClock(server)
			tr := memTransportOf(server)
			processed := tr.processCount()
			read := pollRead(server)
			awaitPolling(t, tr, processed)
			if err := tt.end(server); err != nil {
				t.Fatalf("%s error: %v", tt.name, err)
			}
			if r := await(t, "the polling Read", read); !errors.Is(r.err, tt.want) {
				t.Errorf("the polling Read after the %s = %d, %v; want %v", tt.name, r.n, r.err, tt.want)
			}
		})
	}
}

// PollTime switches the poll: with a positive one a wait polls until the
// test moves the clock on by as much, with zero it looks once and declares
// at once, the clock standing still, as a wait did before there was a poll.
func TestNativePollTimeSwitchesThePoll(t *testing.T) {
	for _, tt := range []struct {
		name string
		poll time.Duration
	}{
		{"positive", testPollTime},
		{"zero", 0},
	} {
		t.Run(tt.name, func(t *testing.T) {
			client, server := nativeOnMemLink.pair(t, Config{PollTime: tt.poll})
			clk := stopPollClock(server)
			tr := memTransportOf(server)
			processed, expects := tr.processCount(), tr.expectCount(arrivals)
			read := pollRead(server)
			if tt.poll > 0 {
				awaitPolling(t, tr, processed)
				clk.advance(tt.poll)
			}
			eventually(t, "the Read declared that it waits for arrivals", func() bool { return tr.expectCount(arrivals) > expects })
			if n := tr.processCount() - processed; tt.poll == 0 && n != 1 {
				t.Errorf("with PollTime %v the Read had the transport process %d times before it declared, want once", tt.poll, n)
			}
			if _, err := client.Write([]byte("x")); err != nil {
				t.Fatalf("Write error: %v", err)
			}
			if r := await(t, "the parked Read", read); r.n != 1 || r.err != nil {
				t.Errorf("the parked Read = %d, %v; want 1 and nil", r.n, r.err)
			}
		})
	}
}

// The polls of a direction back off while they find nothing: after
// pollMisses misses in a row the next pollPause waits do not poll, the one
// after polls again and its miss doubles the pause, and a poll that finds
// the stream ready brings polling back at once, so that one miss then does
// not pause it. What the first look of a wait finds is no poll's find.
func TestNativePollBacksOffUntilItFindsTheStreamReady(t *testing.T) {
	client, server := nativeOnMemLink.pair(t, Config{PollTime: testPollTime})
	p := pollScript{t: t, client: client, server: server, clk: stopPollClock(server), tr: memTransportOf(server)}
	p.miss()
	p.readyAtFirstLook()
	for range pollMisses - 1 {
		p.miss()
	}
	for range pollPause {
		p.unpolled()
	}
	p.miss()
	for range 2 * pollPause {
		p.unpolled()
	}
	p.hit()
	p.miss()
	p.hit()
}

// After its first pause each miss in a row doubles the pause of the polls of
// a direction, pollDoublings times at most.
func TestNativePollPauseDoublesUpToItsCap(t *testing.T) {
	var p pollPace
	for range pollMisses - 1 {
		p.missed()
	}
	for i := range pollDoublings + 2 {
		p.missed()
		want := pollPause << min(i, pollDoublings)
		skipped := 0
		for skipped <= want && !p.due() {
			skipped++
		}
		if skipped != want {
			t.Fatalf("after %d misses in a row %d waits did not poll, want %d", pollMisses+i, skipped, want)
		}
	}
}

// In steady state a Write and the Read of what it wrote allocate nothing
// while the waits poll: twice the rounds cost as many allocations.
func TestNativePollingRoundsDoNotAllocate(t *testing.T) {
	client, server := nativeOnMemLink.pair(t, Config{PollTime: testPollTime})
	const chunk = 1 << 10
	wbuf, rbuf := make([]byte, chunk), make([]byte, chunk)
	readWrite := func(rounds int) float64 {
		return testing.AllocsPerRun(20, func() {
			for range rounds {
				if _, err := client.Write(wbuf); err != nil {
					t.Fatalf("Write error: %v", err)
				}
				if _, err := io.ReadFull(server, rbuf); err != nil {
					t.Fatalf("ReadFull error: %v", err)
				}
			}
		})
	}
	if few, many := readWrite(64), readWrite(128); many != few {
		t.Errorf("Write and Read of 64 and 128 rounds while the waits poll: %v and %v allocations; want as many", few, many)
	}
}

// pollScript drives Reads of one byte from server, whose waits poll by a
// clock that stands still until the script moves it on, and the Writes of
// client that give each Read its byte.
type pollScript struct {
	t              *testing.T
	client, server *Conn
	clk            *pollClock
	tr             *memTransport // server's
}

// miss has a Read poll until the poll time has passed, declare that it
// waits for arrivals and park.
func (p *pollScript) miss() {
	p.t.Helper()
	processed, expects := p.tr.processCount(), p.tr.expectCount(arrivals)
	read := pollRead(p.server)
	awaitPolling(p.t, p.tr, processed)
	p.clk.advance(testPollTime)
	eventually(p.t, "the Read declared after its poll", func() bool { return p.tr.expectCount(arrivals) > expects })
	p.give(read)
}

// hit has a Read poll and find its byte without declaring anything.
func (p *pollScript) hit() {
	p.t.Helper()
	processed, expects := p.tr.processCount(), p.tr.expectCount(arrivals)
	read := pollRead(p.server)
	awaitPolling(p.t, p.tr, processed)
	p.give(read)
	if n := p.tr.expectCount(arrivals) - expects; n != 0 {
		p.t.Fatalf("a Read whose poll found its byte declared %d times that it waits for arrivals, want never", n)
	}
}

// unpolled has a Read look once and declare at once, without polling.
func (p *pollScript) unpolled() {
	p.t.Helper()
	processed, expects := p.tr.processCount(), p.tr.expectCount(arrivals)
	read := pollRead(p.server)
	eventually(p.t, "the Read declared without polling", func() bool { return p.tr.expectCount(arrivals) > expects })
	if n := p.tr.processCount() - processed; n != 1 {
		p.t.Fatalf("a Read that does not poll had the transport process %d times, want once", n)
	}
	p.give(read)
}

// readyAtFirstLook has a Read find its byte at the first look of its wait,
// the byte taken in but not fed to the stream before the Read begins. A
// byte fed and read first takes the arm a parked Read may have left, which
// would have the second fed in the background.
func (p *pollScript) readyAtFirstLook() {
	p.t.Helper()
	if _, err := p.client.Write([]byte("y")); err != nil {
		p.t.Fatalf("Write error: %v", err)
	}
	s := p.server.s.(*nativeStream)
	eventually(p.t, "the first byte fed", func() bool {
		p.tr.process()
		s.mu.Lock()
		defer s.mu.Unlock()
		return s.ready(readable)
	})
	if n, err := p.server.Read(make([]byte, 1)); n != 1 || err != nil {
		p.t.Fatalf("Read of the byte fed = %d, %v; want 1 and nil", n, err)
	}
	processed := p.tr.processCount()
	if _, err := p.client.Write([]byte("x")); err != nil {
		p.t.Fatalf("Write error: %v", err)
	}
	p.tr.takeIn()
	if r := await(p.t, "the Read of the byte taken in", pollRead(p.server)); r.n != 1 || r.err != nil {
		p.t.Fatalf("the Read of the byte taken in = %d, %v; want 1 and nil", r.n, r.err)
	}
	if n := p.tr.processCount() - processed; n != 1 {
		p.t.Fatalf("the Read of the byte taken in had the transport process %d times, want once", n)
	}
}

// give writes the byte the Read read waits for and checks that it gets it.
func (p *pollScript) give(read <-chan copied) {
	p.t.Helper()
	if _, err := p.client.Write([]byte("x")); err != nil {
		p.t.Fatalf("Write error: %v", err)
	}
	if r := await(p.t, "the Read", read); r.n != 1 || r.err != nil {
		p.t.Fatalf("the Read = %d, %v; want 1 and nil", r.n, r.err)
	}
}

// validate takes a zero or positive PollTime and rejects a negative one, as
// it rejects the negative sizes.
func TestConfigPollTime(t *testing.T) {
	for _, poll := range []time.Duration{0, 3 * time.Microsecond} {
		if err := (Config{PollTime: poll}).validate(); err != nil {
			t.Errorf("Config{PollTime: %v}.validate() = %v, want nil", poll, err)
		}
	}
	if err := (Config{PollTime: -time.Nanosecond}).validate(); !errors.Is(err, errConfig) || !strings.Contains(err.Error(), "PollTime") {
		t.Errorf("Config{PollTime: -1ns}.validate() = %v, want an error matching errConfig and naming PollTime", err)
	}
}
