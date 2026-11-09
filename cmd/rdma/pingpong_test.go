package main

import (
	"errors"
	"flag"
	"strings"
	"testing"

	"github.com/wzshiming/rdma"
)

func TestParsePingpong(t *testing.T) {
	tests := []struct {
		name string
		args []string
		want pingpongOptions
		err  string // "" for success, "help" for -h, else the usage error
	}{
		{
			"server with defaults",
			[]string{"-listen", "10.200.0.1:0"},
			pingpongOptions{endpoint: endpoint{listen: "10.200.0.1:0"}, size: 64, n: 1000},
			"",
		},
		{
			"client with every flag",
			[]string{"-connect", "rdma.example:4791", "-size", "4096", "-n", "10", "-msgsize", "8192", "-depth", "64"},
			pingpongOptions{endpoint: endpoint{connect: "rdma.example:4791", msgSize: 8192, depth: 64}, size: 4096, n: 10},
			"",
		},
		{
			"server that keeps serving",
			[]string{"-listen", ":4791", "-keep"},
			pingpongOptions{endpoint: endpoint{listen: ":4791", keep: true}, size: 64, n: 1000},
			"",
		},
		{"-h asks for help", []string{"-h"}, pingpongOptions{}, "help"},
		{"no endpoint", nil, pingpongOptions{}, "one of -listen or -connect is required"},
		{"both endpoints", []string{"-listen", ":1", "-connect", "h:1"}, pingpongOptions{}, "-listen and -connect are mutually exclusive"},
		{"-keep on a client", []string{"-connect", "h:1", "-keep"}, pingpongOptions{}, "-keep applies to -listen"},
		{"zero iterations", []string{"-connect", "h:1", "-n", "0"}, pingpongOptions{}, "-n must be at least 1"},
		{"empty payload", []string{"-connect", "h:1", "-size", "0"}, pingpongOptions{}, "-size must be at least 1"},
		{"negative message size", []string{"-connect", "h:1", "-msgsize", "-1"}, pingpongOptions{}, "-msgsize must not be negative"},
		{"negative depth", []string{"-connect", "h:1", "-depth", "-1"}, pingpongOptions{}, "-depth must not be negative"},
		{"unknown flag", []string{"-connect", "h:1", "-bogus"}, pingpongOptions{}, "flag provided but not defined: -bogus"},
		{"non-numeric -n", []string{"-connect", "h:1", "-n", "many"}, pingpongOptions{}, `invalid value "many" for flag -n`},
		{"stray argument", []string{"-connect", "h:1", "extra"}, pingpongOptions{}, `unexpected argument "extra"`},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			got, _, err := parsePingpong(tt.args)
			checkParse(t, err, tt.err)
			if err == nil && *got != tt.want {
				t.Errorf("parsePingpong(%q) = %+v, want %+v", tt.args, *got, tt.want)
			}
		})
	}
}

// checkParse checks a parseFlags result against the expectation of a table:
// "" for success, "help" for flag.ErrHelp, anything else a usageError with
// that message.
func checkParse(t *testing.T, err error, want string) {
	t.Helper()
	switch want {
	case "":
		if err != nil {
			t.Fatalf("err = %v, want nil", err)
		}
	case "help":
		if !errors.Is(err, flag.ErrHelp) {
			t.Fatalf("err = %v, want flag.ErrHelp", err)
		}
	default:
		var ue usageError
		if !errors.As(err, &ue) {
			t.Fatalf("err = %v (%T), want a usageError", err, err)
		}
		if !strings.Contains(err.Error(), want) {
			t.Fatalf("err = %q, want it to contain %q", err, want)
		}
	}
}

func TestEndpointConfig(t *testing.T) {
	tests := []struct {
		name string
		e    endpoint
		want rdma.Config
	}{
		{"defaults stay zero", endpoint{}, rdma.Config{}},
		{"message size and depth map to the Config", endpoint{msgSize: 8192, depth: 64}, rdma.Config{MessageSize: 8192, ReceiveDepth: 64, SendDepth: 64}},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			if got := tt.e.config(); got != tt.want {
				t.Errorf("%+v.config() = %+v, want %+v", tt.e, got, tt.want)
			}
		})
	}
}

func TestPingpongUsage(t *testing.T) {
	tests := []struct {
		name   string
		args   []string
		code   int
		stdout []string // substrings the standard output must contain
		stderr string
	}{
		{"help lists the flags and the fields", []string{"help", "pingpong"}, exitOK,
			[]string{"Usage: rdma pingpong", "-listen", "-connect", "-size", "-n", "-msgsize", "-depth", "-keep", "iterations", "min", "median", "p99", "max", "msg/s"}, ""},
		{"no endpoint is a usage error", []string{"pingpong"}, exitUsage, nil, "rdma pingpong: one of -listen or -connect is required"},
		{"a bad flag is a usage error", []string{"pingpong", "-connect", "h:1", "-n", "0"}, exitUsage, nil, "-n must be at least 1"},
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
