package main

import (
	"errors"
	"flag"
	"fmt"
	"io"
	"sync"
	"time"

	"github.com/wzshiming/rdma"
)

var bwCommand = &command{
	name:    "bw",
	summary: "measure throughput",
	run:     runBW,
}

const bwUsage = `Usage: rdma bw -listen ip:port | -connect host:port [flags]

Measure the throughput of an RDMA stream. The server (-listen) prints
"listening on ip:port", reads and discards what -conns connections send
until each closes its sending side, and exits. The client (-connect) opens
-conns connections, writes -size bytes at a time on each for -duration, waits
for the server to confirm it has read everything, and prints the statistics
as "key: value" lines, with one line per connection before the aggregate when
there are several:

  connections: 2
  size: 65536
  connection 0: bytes 1744830464 duration 2.001s MiB/s 831.56 Gbit/s 6.976
  connection 1: bytes 1711276032 duration 2.001s MiB/s 815.57 Gbit/s 6.842
  bytes: 3456106496
  duration: 2.001s
  MiB/s: 1647.13
  Gbit/s: 13.817

A connection's duration runs from its first write to the server's
confirmation; the aggregate counts every byte over the longest of them.
MiB/s uses 2^20 bytes, Gbit/s 10^9 bits.
`

type bwOptions struct {
	endpoint
	size     int
	duration time.Duration
	conns    int
}

func (o *bwOptions) flags() *flag.FlagSet {
	fs := newFlagSet("bw")
	o.endpoint.bind(fs)
	fs.IntVar(&o.size, "size", 64<<10, "`bytes` per write")
	fs.DurationVar(&o.duration, "duration", 10*time.Second, "how long to send")
	fs.IntVar(&o.conns, "conns", 1, "connections to serve or to open")
	return fs
}

func (o *bwOptions) check() error {
	if err := o.endpoint.check(); err != nil {
		return err
	}
	switch {
	case o.size < 1:
		return errors.New("-size must be at least 1")
	case o.duration <= 0:
		return errors.New("-duration must be positive")
	case o.conns < 1:
		return errors.New("-conns must be at least 1")
	}
	return nil
}

func parseBW(args []string) (*bwOptions, *flag.FlagSet, error) {
	o := &bwOptions{}
	fs := o.flags()
	return o, fs, parseFlags(fs, args, o.check)
}

func runBW(args []string, stdout, stderr io.Writer) int {
	o, fs, err := parseBW(args)
	if err != nil {
		return reportParse(err, fs, bwUsage, stdout, stderr)
	}
	if o.listen != "" {
		err = o.serve(o.conns, stdout, stderr, sink)
	} else {
		err = o.bw(stdout)
	}
	if err != nil {
		return fail(fs.Name(), err, stderr)
	}
	return exitOK
}

// sink discards what the peer sends until it closes its sending side.
func sink(conn *rdma.Conn) error {
	_, err := io.Copy(io.Discard, conn)
	return err
}

// bw runs the client side: -conns connections streaming at once for
// -duration, then the report.
func (o *bwOptions) bw(stdout io.Writer) error {
	conns := make([]*rdma.Conn, o.conns)
	for i := range conns {
		conn, err := o.dial()
		if err != nil {
			return err
		}
		defer conn.Close()
		conns[i] = conn
	}

	results := make([]throughput, len(conns))
	errs := make([]error, len(conns))
	deadline := time.Now().Add(o.duration)
	var wg sync.WaitGroup
	for i, conn := range conns {
		wg.Add(1)
		go func() {
			defer wg.Done()
			results[i], errs[i] = stream(conn, o.size, deadline)
		}()
	}
	wg.Wait()
	for i, err := range errs {
		if err != nil {
			return fmt.Errorf("connection %d: %w", i, err)
		}
	}
	writeBWReport(stdout, o.size, results)
	return nil
}

// stream writes size bytes at a time until the deadline, closes the sending
// side and waits for the peer's EOF, which confirms it has read everything.
func stream(conn *rdma.Conn, size int, deadline time.Time) (throughput, error) {
	buf := make([]byte, size)
	var n int64
	start := time.Now()
	for time.Now().Before(deadline) {
		w, err := conn.Write(buf)
		n += int64(w)
		if err != nil {
			return throughput{}, err
		}
	}
	if err := conn.CloseWrite(); err != nil {
		return throughput{}, err
	}
	if _, err := io.Copy(io.Discard, conn); err != nil {
		return throughput{}, err
	}
	return throughput{Bytes: n, Duration: time.Since(start)}, nil
}

// writeBWReport writes the throughput of each connection, when there are
// several, and the aggregate: every byte over the longest duration.
func writeBWReport(w io.Writer, size int, conns []throughput) {
	fmt.Fprintf(w, "connections: %d\n", len(conns))
	fmt.Fprintf(w, "size: %d\n", size)
	var total throughput
	for i, c := range conns {
		if len(conns) > 1 {
			fmt.Fprintf(w, "connection %d: bytes %d duration %v MiB/s %.2f Gbit/s %.3f\n", i, c.Bytes, c.Duration, c.mibPerSecond(), c.gbitPerSecond())
		}
		total.Bytes += c.Bytes
		total.Duration = max(total.Duration, c.Duration)
	}
	total.write(w)
}
