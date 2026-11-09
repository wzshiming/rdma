// Command echo is a minimal echo server and client over RDMA streams.
//
// Run the server on one host and the client on another, or on the same one
// over Soft-RoCE:
//
//	echo -listen 10.200.0.1:4791
//	echo -connect 10.200.0.1:4791 -message "hello, rdma"
//
// The server echoes every connection until it is killed; the client sends
// its message as a line and prints the echo.
package main

import (
	"bufio"
	"flag"
	"fmt"
	"io"
	"net"
	"os"

	"github.com/wzshiming/rdma"
)

func main() {
	listen := flag.String("listen", "", "serve echo at this ip:port")
	connect := flag.String("connect", "", "send to the echo server at this host:port")
	message := flag.String("message", "hello, rdma", "line to send")
	flag.Parse()

	var err error
	switch {
	case *listen != "" && *connect == "":
		var ln *rdma.Listener
		if ln, err = rdma.Listen("rdma", *listen); err == nil {
			fmt.Printf("listening on %s\n", ln.Addr())
			err = serve(ln)
		}
	case *connect != "" && *listen == "":
		err = send(*connect, *message, os.Stdout)
	default:
		fmt.Fprintln(os.Stderr, "echo: one of -listen or -connect is required")
		os.Exit(2)
	}
	if err != nil {
		fmt.Fprintln(os.Stderr, "echo:", err)
		os.Exit(1)
	}
}

// serve echoes every connection ln accepts until Accept fails.
func serve(ln net.Listener) error {
	for {
		conn, err := ln.Accept()
		if err != nil {
			return err
		}
		go func() {
			defer conn.Close()
			io.Copy(conn, conn)
		}()
	}
}

// send sends message as a line to the echo server at addr and writes the
// echoed line to w.
func send(addr, message string, w io.Writer) error {
	conn, err := rdma.Dial("rdma", addr)
	if err != nil {
		return err
	}
	defer conn.Close()
	if _, err := fmt.Fprintln(conn, message); err != nil {
		return err
	}
	reply, err := bufio.NewReader(conn).ReadString('\n')
	if err != nil {
		return err
	}
	_, err = io.WriteString(w, reply)
	return err
}
