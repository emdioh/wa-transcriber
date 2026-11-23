package main

import (
	"bytes"
	"strings"
	"testing"
)

// TestRunUsage covers the invocation checks: a bad call is exit 3 and a
// message on stderr, never a panic and never a hang.
func TestRunUsage(t *testing.T) {
	tests := []struct {
		name string
		args []string
		want string
	}{
		{"no command", nil, "missing the command"},
		{"two arguments", []string{"a", "b"}, "one quoted argument"},
		{"empty command", []string{"   "}, "empty"},
		{"zero idle", []string{"-idle", "0", "true"}, "-idle"},
		{"negative prompt", []string{"-prompt", "-1s", "true"}, "-prompt"},
		{"unknown flag", []string{"-nope", "true"}, "flag provided but not defined"},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			var stdout, stderr bytes.Buffer
			code := run(tt.args, &stdout, &stderr)
			if code != exitUsage {
				t.Fatalf("exit status = %d, want %d", code, exitUsage)
			}
			if stdout.Len() != 0 {
				t.Errorf("usage errors must not write to stdout, got %q", stdout.String())
			}
			if !strings.Contains(stderr.String(), tt.want) {
				t.Errorf("stderr = %q, want it to mention %q", stderr.String(), tt.want)
			}
		})
	}
}

// TestRunNoDevice: with a device path that cannot be opened the program is a
// console problem, exit 2, with the device named.
func TestRunNoDevice(t *testing.T) {
	var stdout, stderr bytes.Buffer
	code := run([]string{"-dev", "/nonexistent/ttyACM0", "true"}, &stdout, &stderr)
	if code != exitConsole {
		t.Fatalf("exit status = %d, want %d", code, exitConsole)
	}
	if !strings.Contains(stderr.String(), "/nonexistent/ttyACM0") {
		t.Errorf("stderr = %q, want the device named", stderr.String())
	}
}

// TestRunHelp is not a usage error: -h exits 0.
func TestRunHelp(t *testing.T) {
	var stdout, stderr bytes.Buffer
	if code := run([]string{"-h"}, &stdout, &stderr); code != 0 {
		t.Fatalf("exit status = %d, want 0", code)
	}
	for _, want := range []string{"-dev", "-user", "-idle", "-prompt", "-quiet"} {
		if !strings.Contains(stderr.String(), want) {
			t.Errorf("help must document %s", want)
		}
	}
}
