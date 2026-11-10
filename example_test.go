package rdma_test

import (
	"bufio"
	"context"
	"fmt"
	"io"
	"log"
	"net"
	"time"

	"github.com/wzshiming/rdma"
)

// ExampleDial connects to an RDMA stream server, like net.Dial would to a TCP
// one, and reads its greeting.
func ExampleDial() {
	conn, err := rdma.Dial("rdma", "10.200.0.1:4791")
	if err != nil {
		log.Fatal(err)
	}
	defer conn.Close()

	fmt.Fprintf(conn, "GET / HTTP/1.0\r\n\r\n")
	status, err := bufio.NewReader(conn).ReadString('\n')
	if err != nil {
		log.Fatal(err)
	}
	fmt.Print(status)
}

// ExampleListen serves echo connections, like a TCP server built on
// net.Listen; the connections it accepts are net.Conn values.
func ExampleListen() {
	ln, err := rdma.Listen("rdma", "10.200.0.1:4791")
	if err != nil {
		log.Fatal(err)
	}
	defer ln.Close()

	for {
		conn, err := ln.Accept()
		if err != nil {
			log.Fatal(err)
		}
		go func(c net.Conn) {
			defer c.Close()
			io.Copy(c, c)
		}(conn)
	}
}

// ExampleDialer dials with a deadline, a pinned source address and small
// buffers and queues, then closes its sending side and reads the reply.
func ExampleDialer() {
	d := rdma.Dialer{
		Timeout:   5 * time.Second,
		LocalAddr: &rdma.Addr{IP: net.ParseIP("10.200.0.2")},
		Config:    rdma.Config{SendBufferSize: 16 << 10, ReceiveBufferSize: 16 << 10, SendQueueSize: 16, ReceiveQueueSize: 16},
	}
	conn, err := d.DialContext(context.Background(), "rdma4", "10.200.0.1:4791")
	if err != nil {
		log.Fatal(err)
	}
	defer conn.Close()

	if _, err := conn.Write([]byte("ping")); err != nil {
		log.Fatal(err)
	}
	if err := conn.CloseWrite(); err != nil {
		log.Fatal(err)
	}
	reply, err := io.ReadAll(conn)
	if err != nil {
		log.Fatal(err)
	}
	fmt.Printf("%s\n", reply)
}
