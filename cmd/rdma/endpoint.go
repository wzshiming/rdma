package main

import (
	"context"
	"errors"
	"flag"
	"fmt"
	"io"
	"sync"

	"github.com/wzshiming/rdma"
)

// endpoint holds the flags the measurement commands share: the side to take
// and the connection's buffers.
type endpoint struct {
	listen  string
	connect string
	msgSize int
	depth   int
	keep    bool
}

func (e *endpoint) bind(fs *flag.FlagSet) {
	fs.StringVar(&e.listen, "listen", "", "serve at this `ip:port`; port 0 picks a free one")
	fs.StringVar(&e.connect, "connect", "", "measure against the server at this `host:port`")
	fs.IntVar(&e.msgSize, "msgsize", 0, "message size on the wire in `bytes`, rdma.Config.MessageSize (0: the default, 16 KiB)")
	fs.IntVar(&e.depth, "depth", 0, "`messages` in flight per direction, rdma.Config.ReceiveDepth and SendDepth (0: the default, 32)")
	fs.BoolVar(&e.keep, "keep", false, "keep serving after the first client run")
}

func (e *endpoint) check() error {
	switch {
	case e.listen == "" && e.connect == "":
		return errors.New("one of -listen or -connect is required")
	case e.listen != "" && e.connect != "":
		return errors.New("-listen and -connect are mutually exclusive")
	case e.keep && e.listen == "":
		return errors.New("-keep applies to -listen")
	case e.msgSize < 0:
		return errors.New("-msgsize must not be negative")
	case e.depth < 0:
		return errors.New("-depth must not be negative")
	}
	return nil
}

// config returns the connection buffers the flags ask for.
func (e *endpoint) config() rdma.Config {
	return rdma.Config{MessageSize: e.msgSize, ReceiveDepth: e.depth, SendDepth: e.depth}
}

// dial connects to the server named by -connect.
func (e *endpoint) dial() (*rdma.Conn, error) {
	d := rdma.Dialer{Config: e.config()}
	return d.Dial("rdma", e.connect)
}

// serve listens at -listen, prints the address as "listening on ip:port",
// and serves conns connections at a time with handle, each on its own
// goroutine: one run and then it returns, or run after run with -keep, when
// a failed run goes to stderr and only a failed Accept ends serving.
func (e *endpoint) serve(conns int, stdout, stderr io.Writer, handle func(*rdma.Conn) error) error {
	lc := rdma.ListenConfig{Config: e.config()}
	ln, err := lc.Listen(context.Background(), "rdma", e.listen)
	if err != nil {
		return err
	}
	defer ln.Close()
	fmt.Fprintf(stdout, "listening on %s\n", ln.Addr())
	for {
		failed, err := serveRun(ln, conns, handle)
		if err != nil {
			return err
		}
		if !e.keep {
			return failed
		}
		if failed != nil {
			fmt.Fprintf(stderr, "rdma: %v\n", failed)
		}
	}
}

// serveRun accepts conns connections, handles each on its own goroutine and
// waits for all of them; failed is the first handler error, err a failed
// Accept.
func serveRun(ln *rdma.Listener, conns int, handle func(*rdma.Conn) error) (failed, err error) {
	var wg sync.WaitGroup
	defer wg.Wait()
	errs := make(chan error, conns)
	for i := 0; i < conns; i++ {
		conn, err := ln.AcceptRDMA()
		if err != nil {
			return nil, err
		}
		wg.Add(1)
		go func() {
			defer wg.Done()
			defer conn.Close()
			errs <- handle(conn)
		}()
	}
	wg.Wait()
	close(errs)
	for err := range errs {
		if err != nil {
			return err, nil
		}
	}
	return nil, nil
}
