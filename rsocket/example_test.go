package rsocket_test

import (
	"fmt"
	"io"
	"log"
	"net/netip"
	"os"
	"time"

	"github.com/wzshiming/rdma/rsocket"
)

// Example connects to a server, sends a request and reads the reply with
// blocking calls.
func Example() {
	s, err := rsocket.New(rsocket.IPv4)
	if err != nil {
		log.Fatal(err)
	}
	defer s.Close()
	if err := s.Connect(netip.MustParseAddrPort("10.200.0.1:7471")); err != nil {
		log.Fatal(err)
	}
	if _, err := s.Write([]byte("hello")); err != nil {
		log.Fatal(err)
	}
	// Shutdown(ShutWrite) gives the server EOF; its reply ends with its own.
	if err := s.Shutdown(rsocket.ShutWrite); err != nil {
		log.Fatal(err)
	}
	reply, err := io.ReadAll(s)
	if err != nil {
		log.Fatal(err)
	}
	fmt.Printf("%s\n", reply)
}

// ExampleSocket_Accept serves one connection, echoing what it reads.
func ExampleSocket_Accept() {
	ln, err := rsocket.New(rsocket.IPv4)
	if err != nil {
		log.Fatal(err)
	}
	defer ln.Close()
	if err := ln.Bind(netip.MustParseAddrPort("10.200.0.1:7471")); err != nil {
		log.Fatal(err)
	}
	if err := ln.Listen(8); err != nil {
		log.Fatal(err)
	}
	conn, peer, err := ln.Accept()
	if err != nil {
		log.Fatal(err)
	}
	defer conn.Close()
	fmt.Println("connection from", peer)
	if _, err := io.Copy(conn, conn); err != nil {
		log.Fatal(err)
	}
}

// ExamplePoll waits on a non-blocking socket and a pipe at once: a byte
// written to the pipe wakes the wait before any data arrives.
func ExamplePoll() {
	s, err := rsocket.New(rsocket.IPv4)
	if err != nil {
		log.Fatal(err)
	}
	defer s.Close()
	if err := s.SetNonblock(true); err != nil {
		log.Fatal(err)
	}
	// Connect reports syscall.EINPROGRESS here; Poll with PollOut tells when
	// the attempt has ended and SocketError how.
	if err := s.Connect(netip.MustParseAddrPort("10.200.0.1:7471")); err != nil {
		fds := []rsocket.PollFD{{Fd: s.Descriptor(), Events: rsocket.PollOut}}
		if _, err := rsocket.Poll(fds, 10*time.Second); err != nil {
			log.Fatal(err)
		}
		if err := s.SocketError(); err != nil {
			log.Fatal(err)
		}
	}

	wakeR, wakeW, err := os.Pipe()
	if err != nil {
		log.Fatal(err)
	}
	defer wakeR.Close()
	defer wakeW.Close()
	go func() {
		time.Sleep(time.Second)
		wakeW.Write([]byte{0})
	}()

	fds := []rsocket.PollFD{
		{Fd: s.Descriptor(), Events: rsocket.PollIn},
		{Fd: int(wakeR.Fd()), Events: rsocket.PollIn},
	}
	if _, err := rsocket.Poll(fds, -1); err != nil {
		log.Fatal(err)
	}
	switch {
	case fds[1].Revents&rsocket.PollIn != 0:
		fmt.Println("woken up")
	case fds[0].Revents&rsocket.PollIn != 0:
		buf := make([]byte, 4096)
		n, err := s.Read(buf)
		if err != nil {
			log.Fatal(err)
		}
		fmt.Printf("%d bytes\n", n)
	}
}
