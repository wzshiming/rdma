//go:build linux && !android && (amd64 || arm64 || loong64 || ppc64le || riscv64 || s390x)

package ucma

import (
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"sync"
	"syscall"
	"testing"
	"time"
)

// A missing device node, as on a host without the rdma_ucm module, is the
// errno open(2) returned, not a wrapper around it.
func TestOpenMissingNodeIsTheOpenErrno(t *testing.T) {
	c, err := open(filepath.Join(t.TempDir(), "rdma_cm"))
	if err != syscall.ENOENT {
		t.Fatalf("open() = %v, %#v; want syscall.ENOENT", c, err)
	}
}

// The channel's descriptor is open for reading and writing, non-blocking and
// closed on exec.
func TestOpenFlags(t *testing.T) {
	c, err := open(os.DevNull)
	if err != nil {
		t.Fatalf("open(%s) error: %v", os.DevNull, err)
	}
	defer c.Close()
	var status, fdFlags uintptr
	var errno1, errno2 syscall.Errno
	if err := c.raw.Control(func(fd uintptr) {
		status, _, errno1 = syscall.Syscall(syscall.SYS_FCNTL, fd, syscall.F_GETFL, 0)
		fdFlags, _, errno2 = syscall.Syscall(syscall.SYS_FCNTL, fd, syscall.F_GETFD, 0)
	}); err != nil {
		t.Fatal(err)
	}
	if errno1 != 0 || errno2 != 0 {
		t.Fatalf("fcntl: %v, %v", errno1, errno2)
	}
	if status&syscall.O_ACCMODE != syscall.O_RDWR || status&syscall.O_NONBLOCK == 0 || fdFlags&syscall.FD_CLOEXEC == 0 {
		t.Errorf("file status %#o, descriptor flags %#x; want O_RDWR|O_NONBLOCK and FD_CLOEXEC", status, fdFlags)
	}
}

func TestCloseTwice(t *testing.T) {
	c, err := open(os.DevNull)
	if err != nil {
		t.Fatal(err)
	}
	if err := c.Close(); err != nil {
		t.Fatalf("Close() error: %v", err)
	}
	if err := c.Close(); !errors.Is(err, os.ErrClosed) {
		t.Errorf("second Close() error = %v, want os.ErrClosed", err)
	}
}

// fileID returns what identifies the file behind a descriptor.
func fileID(t *testing.T, fd uintptr) [2]uint64 {
	t.Helper()
	var st syscall.Stat_t
	if err := syscall.Fstat(int(fd), &st); err != nil {
		t.Fatal(err)
	}
	return [2]uint64{uint64(st.Dev), st.Ino}
}

// A command running while the channel closes completes on the channel's
// descriptor, and Close returns after it.
func TestCloseWaitsForARunningCommand(t *testing.T) {
	d := newFakeDevice(t)
	want := fileID(t, fdOf(t, d.ch))
	entered, release := make(chan struct{}), make(chan struct{})
	var got syscall.Stat_t
	var statErr error
	d.setReply(func(fd uintptr, cmd []byte) (int, syscall.Errno) {
		close(entered)
		<-release
		statErr = syscall.Fstat(int(fd), &got)
		return len(cmd), 0
	})
	cmdErr := make(chan error, 1)
	go func() { cmdErr <- d.ch.Disconnect(7) }()
	<-entered
	closed := make(chan error, 1)
	go func() { closed <- d.ch.Close() }()
	select {
	case err := <-closed:
		t.Fatalf("Close() = %v while a command was running", err)
	case <-time.After(100 * time.Millisecond):
	}
	close(release)
	if err := <-cmdErr; err != nil {
		t.Errorf("Disconnect() error: %v", err)
	}
	if err := <-closed; err != nil {
		t.Errorf("Close() error: %v", err)
	}
	if id := [2]uint64{uint64(got.Dev), got.Ino}; statErr != nil || id != want {
		t.Errorf("the command wrote to file %v (%v), want the channel's %v", id, statErr, want)
	}
}

// Commands racing Close either complete on the channel's descriptor or
// return os.ErrClosed: none writes to a file that takes the descriptor's
// number once it is closed.
func TestCommandsRacingCloseKeepToTheirDescriptor(t *testing.T) {
	for range 20 {
		d := newFakeDevice(t)
		want := fileID(t, fdOf(t, d.ch))
		var mu sync.Mutex
		var wrong []error
		d.setReply(func(fd uintptr, cmd []byte) (int, syscall.Errno) {
			var st syscall.Stat_t
			if err := syscall.Fstat(int(fd), &st); err != nil || [2]uint64{uint64(st.Dev), st.Ino} != want {
				mu.Lock()
				wrong = append(wrong, fmt.Errorf("descriptor %d: file %d/%d, %v", fd, st.Dev, st.Ino, err))
				mu.Unlock()
			}
			return len(cmd), 0
		})
		var wg sync.WaitGroup
		for range 4 {
			wg.Go(func() {
				for {
					err := d.ch.Disconnect(7)
					if err == os.ErrClosed {
						return
					}
					if err != nil {
						t.Errorf("Disconnect() error: %v", err)
						return
					}
				}
			})
		}
		time.Sleep(time.Millisecond)
		if err := d.ch.Close(); err != nil {
			t.Fatal(err)
		}
		// Take the freed descriptor numbers while the commands still run.
		var files []*os.File
		for range 8 {
			r, w, err := os.Pipe()
			if err != nil {
				t.Fatal(err)
			}
			files = append(files, r, w)
		}
		wg.Wait()
		for _, f := range files {
			f.Close()
		}
		for _, err := range wrong {
			t.Error(err)
		}
	}
}
