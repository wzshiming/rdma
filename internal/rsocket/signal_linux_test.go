//go:build linux && !android && (amd64 || arm64 || loong64 || ppc64le || riscv64 || s390x)

package rsocket_test

import (
	"os"
	"os/signal"
	"strconv"
	"syscall"
	"testing"
	"time"
)

// threadIDs returns the threads of the process.
func threadIDs(t *testing.T) []int {
	t.Helper()
	entries, err := os.ReadDir("/proc/self/task")
	if err != nil {
		t.Fatalf("ReadDir(/proc/self/task) error: %v", err)
	}
	tids := make([]int, 0, len(entries))
	for _, e := range entries {
		tid, err := strconv.Atoi(e.Name())
		if err != nil {
			t.Fatalf("thread %q: %v", e.Name(), err)
		}
		tids = append(tids, tid)
	}
	return tids
}

// TestSignalsReachNotifyWithLibraryThreads checks that, while a connection
// keeps the service threads of librdmacm alive, a signal sent to each
// thread of the process in turn, those threads included, and then a batch
// sent to the process are all received through signal.Notify and the
// process survives. The library's threads were not created by Go, so the
// runtime handles a signal they get on a spare M, which a binary built
// without cgo has only through purego.
func TestSignalsReachNotifyWithLibraryThreads(t *testing.T) {
	before := len(threadIDs(t))
	pair(t)
	tids := threadIDs(t)
	t.Logf("%d threads before the connection, %d with it", before, len(tids))

	ch := make(chan os.Signal, 1)
	signal.Notify(ch, syscall.SIGUSR1)
	defer signal.Stop(ch)
	await := func(what string) {
		t.Helper()
		select {
		case <-ch:
		case <-time.After(5 * time.Second):
			t.Fatalf("SIGUSR1 %s was not delivered within 5s", what)
		}
	}
	pid := os.Getpid()
	sent := 0
	for _, tid := range tids {
		if err := syscall.Tgkill(pid, tid, syscall.SIGUSR1); err != nil {
			if err == syscall.ESRCH {
				continue // the thread has exited since the listing
			}
			t.Fatalf("tgkill(%d, SIGUSR1) error: %v", tid, err)
		}
		sent++
		await("sent to thread " + strconv.Itoa(tid))
	}
	const batch = 20
	for i := range batch {
		if err := syscall.Kill(pid, syscall.SIGUSR1); err != nil {
			t.Fatalf("kill(SIGUSR1) #%d error: %v", i, err)
		}
		await("#" + strconv.Itoa(i) + " sent to the process")
	}
	t.Logf("%d signals sent to the threads one by one and %d to the process were all received", sent, batch)
}
