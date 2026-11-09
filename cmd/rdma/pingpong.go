package main

import (
	"bytes"
	"errors"
	"flag"
	"fmt"
	"io"
	"time"

	"github.com/wzshiming/rdma"
)

var pingpongCommand = &command{
	name:    "pingpong",
	summary: "measure round-trip latency",
	run:     runPingpong,
}

const pingpongUsage = `Usage: rdma pingpong -listen ip:port | -connect host:port [flags]

Measure the round-trip latency of an RDMA stream. The server (-listen)
prints "listening on ip:port", echoes what one client sends until the client
closes its sending side, and exits. The client (-connect) sends -n messages
of -size bytes, each after the echo of the previous one has arrived, and
prints the statistics as "key: value" lines:

  size: 64
  iterations: 1000
  min: 24.204µs
  median: 67.912µs
  p99: 806.728µs
  max: 1.877701ms
  msg/s: 10417.54

Round trips are timed one by one; median and p99 are nearest-rank
percentiles of the sorted samples, msg/s the round trips per second over the
whole run.
`

type pingpongOptions struct {
	endpoint
	size int
	n    int
}

func (o *pingpongOptions) flags() *flag.FlagSet {
	fs := newFlagSet("pingpong")
	o.endpoint.bind(fs)
	fs.IntVar(&o.size, "size", 64, "payload `bytes` per message")
	fs.IntVar(&o.n, "n", 1000, "round trips to measure")
	return fs
}

func (o *pingpongOptions) check() error {
	if err := o.endpoint.check(); err != nil {
		return err
	}
	switch {
	case o.size < 1:
		return errors.New("-size must be at least 1")
	case o.n < 1:
		return errors.New("-n must be at least 1")
	}
	return nil
}

func parsePingpong(args []string) (*pingpongOptions, *flag.FlagSet, error) {
	o := &pingpongOptions{}
	fs := o.flags()
	return o, fs, parseFlags(fs, args, o.check)
}

func runPingpong(args []string, stdout, stderr io.Writer) int {
	o, fs, err := parsePingpong(args)
	if err != nil {
		return reportParse(err, fs, pingpongUsage, stdout, stderr)
	}
	if o.listen != "" {
		err = o.serve(1, stdout, stderr, echo)
	} else {
		err = o.pingpong(stdout)
	}
	if err != nil {
		return fail(fs.Name(), err, stderr)
	}
	return exitOK
}

// echo writes back whatever the peer sends until it closes its sending side.
func echo(conn *rdma.Conn) error {
	_, err := io.Copy(conn, conn)
	return err
}

// pingpong runs the client side: n round trips, then the statistics.
func (o *pingpongOptions) pingpong(stdout io.Writer) error {
	conn, err := o.dial()
	if err != nil {
		return err
	}
	defer conn.Close()

	msg := make([]byte, o.size)
	for i := range msg {
		msg[i] = byte(i)
	}
	reply := make([]byte, o.size)
	samples := make([]time.Duration, o.n)
	start := time.Now()
	for i := range samples {
		t := time.Now()
		if _, err := conn.Write(msg); err != nil {
			return err
		}
		if _, err := io.ReadFull(conn, reply); err != nil {
			return err
		}
		samples[i] = time.Since(t)
		if !bytes.Equal(reply, msg) {
			return fmt.Errorf("round trip %d: the echo differs from the message", i+1)
		}
	}
	elapsed := time.Since(start)
	// Reading the server's EOF lets it finish before the statistics appear.
	if err := conn.CloseWrite(); err != nil {
		return err
	}
	if _, err := io.Copy(io.Discard, conn); err != nil {
		return err
	}

	stats := summarizeLatency(samples, elapsed)
	fmt.Fprintf(stdout, "size: %d\n", o.size)
	stats.write(stdout)
	return nil
}
