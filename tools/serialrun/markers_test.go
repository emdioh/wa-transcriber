package main

import "testing"

func testMarkers() markers {
	return markers{begin: "__SRUN_B__abc123", end: "__SRUN_E__abc123"}
}

// TestMarkersCommandHash: an unquoted '#' would comment out the closing brace
// and the END tag, so such a command is handed to a nested shell where the
// '#' is data. The END tag must stay on the same logical line.
func TestMarkersCommandHash(t *testing.T) {
	m := testMarkers()
	got := m.command("echo a # b")
	want := "echo __SRUN_B__abc123; { sh -c 'echo a # b'; }; echo __SRUN_E__abc123:$?\r"
	if got != want {
		t.Fatalf("got %q\nwant %q", got, want)
	}
}

func TestShellQuote(t *testing.T) {
	tests := []struct{ in, want string }{
		{"echo hi", "'echo hi'"},
		{"echo 'x'", `'echo '\''x'\'''`},
		{"", "''"},
	}
	for _, tt := range tests {
		if got := shellQuote(tt.in); got != tt.want {
			t.Errorf("shellQuote(%q) = %q, want %q", tt.in, got, tt.want)
		}
	}
}

func TestMarkersCommand(t *testing.T) {
	m := testMarkers()
	got := m.command("echo hi")
	want := "echo __SRUN_B__abc123; { echo hi; }; echo __SRUN_E__abc123:$?\r"
	if got != want {
		t.Fatalf("got %q\nwant %q", got, want)
	}
}

func TestMarkersParseEnd(t *testing.T) {
	m := testMarkers()
	tests := []struct {
		name      string
		line      string
		status    int
		real      bool
		lookalike bool
	}{
		{"real zero", "__SRUN_E__abc123:0", 0, true, false},
		{"real three", "__SRUN_E__abc123:3", 3, true, false},
		{"real 127", "__SRUN_E__abc123:127", 127, true, false},
		{"bare tag is not ours", "__SRUN_E__abc123", 0, false, true},
		{"empty status", "__SRUN_E__abc123:", 0, false, true},
		{"status not numeric", "__SRUN_E__abc123:oops", 0, false, true},
		{"status with trailing text", "__SRUN_E__abc123:0 extra", 0, false, true},
		{"no nonce at all", "__SRUN_E__:0", 0, false, false},
		{"wrong nonce", "__SRUN_E__deadbee:0", 0, false, false},
		{"echo of the frame is not a tag line", "echo __SRUN_B__abc123; { echo hi", 0, false, false},
		{"leading whitespace is not the tag", " __SRUN_E__abc123:0", 0, false, false},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			status, real, lookalike := m.parseEnd(tt.line)
			if real != tt.real || lookalike != tt.lookalike || (real && status != tt.status) {
				t.Fatalf("parseEnd(%q) = (%d, %v, %v), want (%d, %v, %v)",
					tt.line, status, real, lookalike, tt.status, tt.real, tt.lookalike)
			}
		})
	}
}

func TestMarkersIsBegin(t *testing.T) {
	m := testMarkers()
	if !m.isBegin("__SRUN_B__abc123") {
		t.Error("the real begin tag must match")
	}
	for _, line := range []string{
		"__SRUN_B__abc123; { echo hi; }",
		"__SRUN_B__",
		" __SRUN_B__abc123",
		"__SRUN_E__abc123:0",
	} {
		if m.isBegin(line) {
			t.Errorf("%q must not be taken for the begin tag", line)
		}
	}
}

// TestMarkersNonceDiffers guards the property the fake-tag test in the e2e
// run depends on: two sessions do not share a tag.
func TestMarkersNonceDiffers(t *testing.T) {
	seen := map[string]bool{}
	for i := 0; i < 64; i++ {
		n, err := newNonce()
		if err != nil {
			t.Fatal(err)
		}
		if len(n) != nonceBytes*2 {
			t.Fatalf("nonce %q has %d chars, want %d", n, len(n), nonceBytes*2)
		}
		if seen[n] {
			t.Fatalf("nonce %q repeated", n)
		}
		seen[n] = true
	}
}
