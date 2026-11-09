package main

import (
	"strings"
	"testing"
)

func TestDevicesUsage(t *testing.T) {
	tests := []struct {
		name   string
		args   []string
		code   int
		stdout string
		stderr string
	}{
		{"-h prints the usage", []string{"devices", "-h"}, exitOK, "Usage: rdma devices", ""},
		{"help devices prints the usage", []string{"help", "devices"}, exitOK, "Usage: rdma devices", ""},
		{"an argument is a usage error", []string{"devices", "rxe0"}, exitUsage, "", `unexpected argument "rxe0"`},
		{"an unknown flag is a usage error", []string{"devices", "-verbose"}, exitUsage, "", "flag provided but not defined: -verbose"},
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
