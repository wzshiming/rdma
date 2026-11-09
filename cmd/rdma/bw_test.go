package main

import (
	"bytes"
	"strings"
	"testing"
	"time"
)

func TestParseBW(t *testing.T) {
	tests := []struct {
		name string
		args []string
		want bwOptions
		err  string
	}{
		{
			"server with defaults",
			[]string{"-listen", "10.200.0.1:0"},
			bwOptions{endpoint: endpoint{listen: "10.200.0.1:0"}, size: 64 << 10, duration: 10 * time.Second, conns: 1},
			"",
		},
		{
			"client with every flag",
			[]string{"-connect", "rdma.example:4791", "-size", "1048576", "-duration", "2s", "-conns", "4", "-msgsize", "65536", "-depth", "128"},
			bwOptions{endpoint: endpoint{connect: "rdma.example:4791", msgSize: 65536, depth: 128}, size: 1 << 20, duration: 2 * time.Second, conns: 4},
			"",
		},
		{"-h asks for help", []string{"-h"}, bwOptions{}, "help"},
		{"no endpoint", nil, bwOptions{}, "one of -listen or -connect is required"},
		{"both endpoints", []string{"-listen", ":1", "-connect", "h:1"}, bwOptions{}, "-listen and -connect are mutually exclusive"},
		{"empty payload", []string{"-connect", "h:1", "-size", "0"}, bwOptions{}, "-size must be at least 1"},
		{"zero duration", []string{"-connect", "h:1", "-duration", "0"}, bwOptions{}, "-duration must be positive"},
		{"malformed duration", []string{"-connect", "h:1", "-duration", "2"}, bwOptions{}, `invalid value "2" for flag -duration`},
		{"zero connections", []string{"-connect", "h:1", "-conns", "0"}, bwOptions{}, "-conns must be at least 1"},
		{"pingpong's -n is unknown", []string{"-connect", "h:1", "-n", "10"}, bwOptions{}, "flag provided but not defined: -n"},
		{"stray argument", []string{"-connect", "h:1", "extra"}, bwOptions{}, `unexpected argument "extra"`},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			got, _, err := parseBW(tt.args)
			checkParse(t, err, tt.err)
			if err == nil && *got != tt.want {
				t.Errorf("parseBW(%q) = %+v, want %+v", tt.args, *got, tt.want)
			}
		})
	}
}

func TestBWUsage(t *testing.T) {
	tests := []struct {
		name   string
		args   []string
		code   int
		stdout []string
		stderr string
	}{
		{"help lists the flags and the fields", []string{"help", "bw"}, exitOK,
			[]string{"Usage: rdma bw", "-listen", "-connect", "-size", "-duration", "-conns", "-msgsize", "-depth", "-keep", "connections", "bytes", "duration", "MiB/s", "Gbit/s"}, ""},
		{"no endpoint is a usage error", []string{"bw"}, exitUsage, nil, "rdma bw: one of -listen or -connect is required"},
		{"a bad flag is a usage error", []string{"bw", "-connect", "h:1", "-conns", "0"}, exitUsage, nil, "-conns must be at least 1"},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			code, stdout, stderr := runCLI(t, tt.args...)
			if code != tt.code {
				t.Errorf("run(%q) = %d, want %d", tt.args, code, tt.code)
			}
			for _, s := range tt.stdout {
				if !strings.Contains(stdout, s) {
					t.Errorf("stdout lacks %q:\n%s", s, stdout)
				}
			}
			if !strings.Contains(stderr, tt.stderr) {
				t.Errorf("stderr = %q, want it to contain %q", stderr, tt.stderr)
			}
		})
	}
}

func TestWriteBWReport(t *testing.T) {
	tests := []struct {
		name  string
		size  int
		conns []throughput
		want  string
	}{
		{
			"one connection is the aggregate",
			65536,
			[]throughput{{Bytes: 1 << 30, Duration: time.Second}},
			"connections: 1\nsize: 65536\nbytes: 1073741824\nduration: 1s\nMiB/s: 1024.00\nGbit/s: 8.590\n",
		},
		{
			"several connections get a line each before the aggregate",
			4096,
			[]throughput{{Bytes: 1 << 30, Duration: time.Second}, {Bytes: 1 << 30, Duration: 2 * time.Second}},
			"connections: 2\nsize: 4096\n" +
				"connection 0: bytes 1073741824 duration 1s MiB/s 1024.00 Gbit/s 8.590\n" +
				"connection 1: bytes 1073741824 duration 2s MiB/s 512.00 Gbit/s 4.295\n" +
				"bytes: 2147483648\nduration: 2s\nMiB/s: 1024.00\nGbit/s: 8.590\n",
		},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			var buf bytes.Buffer
			writeBWReport(&buf, tt.size, tt.conns)
			if buf.String() != tt.want {
				t.Errorf("writeBWReport() wrote\n%s\nwant\n%s", buf.String(), tt.want)
			}
		})
	}
}
