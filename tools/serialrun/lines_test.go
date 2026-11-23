package main

import (
	"bytes"
	"strings"
	"testing"
)

// feedAll pushes data through a single lines value and returns everything it
// produced, including the unterminated remainder if any.
func feedAll(t *testing.T, data string) []string {
	t.Helper()
	var l lines
	out := l.feed([]byte(data))
	if rest, ok := l.flush(); ok {
		out = append(out, rest)
	}
	return out
}

func TestLinesReassembly(t *testing.T) {
	tests := []struct {
		name string
		in   string
		want []string
	}{
		{"bare newline", "a\nb\n", []string{"a", "b"}},
		{"bare carriage return", "a\rb\r", []string{"a", "b"}},
		{"board line endings", "a\r\r\nb\r\r\n", []string{"a", "b"}},
		{"mixed endings", "a\nb\rc\r\r\nd", []string{"a", "b", "c", "d"}},
		{"leading separators make no empty lines", "\r\n\r\nlogin:", []string{"login:"}},
		{"no terminator", "partial", []string{"partial"}},
		{"empty", "", nil},
		{"nul bytes dropped", "a\x00b\r", []string{"ab"}},
		{"internal spaces kept", "  two  spaces  \r\r\n", []string{"  two  spaces  "}},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			got := feedAll(t, tt.in)
			if len(got) != len(tt.want) {
				t.Fatalf("got %q, want %q", got, tt.want)
			}
			for i := range got {
				if got[i] != tt.want[i] {
					t.Errorf("line %d: got %q, want %q", i, got[i], tt.want[i])
				}
			}
		})
	}
}

// TestLinesPartialChunks is the case the serial console hits constantly: a
// chunk boundary in the middle of a line break. "\r" alone must not produce a
// line, and the "\r\n" that follows must not produce an empty one.
func TestLinesPartialChunks(t *testing.T) {
	chunks := []string{"boot", "\r", "\n", "login:", "\r\r", "\n", "# "}
	var l lines
	var got []string
	for _, c := range chunks {
		got = append(got, l.feed([]byte(c))...)
	}
	if rest, ok := l.flush(); ok {
		got = append(got, rest)
	}
	want := []string{"boot", "login:", "# "}
	if strings.Join(got, "|") != strings.Join(want, "|") {
		t.Fatalf("got %q, want %q", got, want)
	}
}

func TestLinesMaxLineBoundsBuffer(t *testing.T) {
	var l lines
	got := l.feed(bytes.Repeat([]byte("x"), maxLine*2+1))
	if len(got) < 2 {
		t.Fatalf("a %.0f-byte line with no terminator must be cut, got %d lines", float64(maxLine*2+1), len(got))
	}
	if len(l.buf) > maxLine {
		t.Fatalf("buffer grew to %d bytes, cap is %d", len(l.buf), maxLine)
	}
}
