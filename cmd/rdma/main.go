// Command rdma inspects RDMA devices and measures RDMA byte streams with the
// rdma package.
//
// Usage:
//
//	rdma <command> [flags]
//
// The commands are devices, which lists the RDMA devices of the host with
// their ports and GID tables, pingpong, which measures the round-trip latency
// of a stream, and bw, which measures its throughput. The measurement
// commands run as a server with -listen on one host and as a client with
// -connect on another (or the same) host; the server prints the address it
// listens on as its first line, serves one client run and exits, and the
// client prints the statistics as "key: value" lines.
//
// The exit status is 0 on success, 1 when a command fails at run time and 2
// for a command line that makes no sense.
package main

import (
	"fmt"
	"io"
	"os"
)

// Exit statuses.
const (
	exitOK    = 0
	exitError = 1
	exitUsage = 2
)

// command is a subcommand; run receives the arguments after the command's
// name and returns the exit status.
type command struct {
	name    string
	summary string
	run     func(args []string, stdout, stderr io.Writer) int
}

var commands []*command

func main() {
	os.Exit(run(os.Args[1:], os.Stdout, os.Stderr))
}

// run dispatches args to a command and returns the exit status.
func run(args []string, stdout, stderr io.Writer) int {
	if len(args) == 0 {
		usage(stderr)
		return exitUsage
	}
	switch args[0] {
	case "help", "-h", "-help", "--help":
		if len(args) == 1 {
			usage(stdout)
			return exitOK
		}
		if c := lookup(args[1]); c != nil {
			return c.run([]string{"-h"}, stdout, stderr)
		}
		return unknown(args[1], stderr)
	}
	c := lookup(args[0])
	if c == nil {
		return unknown(args[0], stderr)
	}
	return c.run(args[1:], stdout, stderr)
}

func lookup(name string) *command {
	for _, c := range commands {
		if c.name == name {
			return c
		}
	}
	return nil
}

func unknown(name string, stderr io.Writer) int {
	fmt.Fprintf(stderr, "rdma: unknown command %q\n", name)
	usage(stderr)
	return exitUsage
}

func usage(w io.Writer) {
	fmt.Fprintln(w, "Usage: rdma <command> [flags]")
	fmt.Fprintln(w)
	fmt.Fprintln(w, "Commands:")
	for _, c := range commands {
		fmt.Fprintf(w, "  %-9s %s\n", c.name, c.summary)
	}
	fmt.Fprintln(w)
	fmt.Fprintln(w, "Run 'rdma help <command>' for the flags of a command.")
}
