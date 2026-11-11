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

// endpoint holds the flags the measurement commands share: the side to take,
// the engine and the rsockets options of the connections.
type endpoint struct {
	listen  string
	connect string
	engine  string
	sndbuf  int
	rcvbuf  int
	sqsize  int
	rqsize  int
	inline  int
	keep    bool
}

func (e *endpoint) bind(fs *flag.FlagSet) {
	fs.StringVar(&e.listen, "listen", "", "serve at this `ip:port`; port 0 picks a free one")
	fs.StringVar(&e.connect, "connect", "", "measure against the server at this `host:port`")
	fs.StringVar(&e.engine, "engine", "", "the `engine` of the connections, native or librdmacm, rdma.Config.Engine (empty: the library's default, native)")
	fs.IntVar(&e.sndbuf, "sndbuf", 0, "send buffer size in `bytes`, rdma.Config.SendBufferSize (0: the rsockets default)")
	fs.IntVar(&e.rcvbuf, "rcvbuf", 0, "receive buffer size in `bytes`, rdma.Config.ReceiveBufferSize (0: the rsockets default)")
	fs.IntVar(&e.sqsize, "sqsize", 0, "send queue depth in `entries`, rdma.Config.SendQueueSize (0: the rsockets default)")
	fs.IntVar(&e.rqsize, "rqsize", 0, "receive queue depth in `entries`, rdma.Config.ReceiveQueueSize (0: the rsockets default)")
	fs.IntVar(&e.inline, "inline", 0, "largest send posted inline in `bytes`, rdma.Config.MaxInline (0: the rsockets default)")
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
	}
	switch rdma.Engine(e.engine) {
	case "", rdma.EngineNative, rdma.EngineLibrdmacm:
	default:
		return fmt.Errorf("-engine must be %s or %s", rdma.EngineNative, rdma.EngineLibrdmacm)
	}
	for _, f := range []struct {
		name  string
		value int
	}{{"sndbuf", e.sndbuf}, {"rcvbuf", e.rcvbuf}, {"sqsize", e.sqsize}, {"rqsize", e.rqsize}, {"inline", e.inline}} {
		if f.value < 0 {
			return fmt.Errorf("-%s must not be negative", f.name)
		}
	}
	return nil
}

// config returns the engine and the rsockets options the flags ask for.
func (e *endpoint) config() rdma.Config {
	return rdma.Config{Engine: rdma.Engine(e.engine), SendBufferSize: e.sndbuf, ReceiveBufferSize: e.rcvbuf, SendQueueSize: e.sqsize, ReceiveQueueSize: e.rqsize, MaxInline: e.inline}
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
// Accept, which returns at once.
func serveRun(ln *rdma.Listener, conns int, handle func(*rdma.Conn) error) (failed, err error) {
	var wg sync.WaitGroup
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
