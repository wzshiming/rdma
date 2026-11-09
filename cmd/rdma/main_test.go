package main

import (
	"bytes"
	"strings"
	"testing"
)

// runCLI runs the command in-process and returns its exit status and output.
func runCLI(t *testing.T, args ...string) (code int, stdout, stderr string) {
	t.Helper()
	var out, errOut bytes.Buffer
	code = run(args, &out, &errOut)
	return code, out.String(), errOut.String()
}

func TestRunDispatch(t *testing.T) {
	tests := []struct {
		name   string
		args   []string
		code   int
		stdout string // substring the standard output must contain
		stderr string // substring the standard error must contain
	}{
		{"no command prints usage and fails", nil, exitUsage, "", "Usage: rdma <command>"},
		{"unknown command fails", []string{"bogus"}, exitUsage, "", `unknown command "bogus"`},
		{"help succeeds", []string{"help"}, exitOK, "Commands:", ""},
		{"-h is help", []string{"-h"}, exitOK, "Commands:", ""},
		{"help of an unknown command fails", []string{"help", "bogus"}, exitUsage, "", `unknown command "bogus"`},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			code, stdout, stderr := runCLI(t, tt.args...)
			if code != tt.code {
				t.Errorf("run(%q) = %d, want %d", tt.args, code, tt.code)
			}
			if !strings.Contains(stdout, tt.stdout) {
				t.Errorf("stdout = %q, want it to contain %q", stdout, tt.stdout)
			}
			if !strings.Contains(stderr, tt.stderr) {
				t.Errorf("stderr = %q, want it to contain %q", stderr, tt.stderr)
			}
		})
	}
}
