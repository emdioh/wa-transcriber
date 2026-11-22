package update

import (
	"os"
	"path/filepath"
	"testing"
)

func ownKey(t *testing.T) string {
	t.Helper()
	k, err := ownAssetKey()
	if err != nil {
		t.Fatalf("ownAssetKey: %v", err)
	}
	return k
}

// otherKey is an asset key of a different platform than the test machine.
func otherKey(t *testing.T) string {
	t.Helper()
	switch ownKey(t) {
	case "linux-amd64":
		return "linux-armv7"
	case "linux-arm64":
		return "linux-amd64"
	default:
		return "linux-amd64"
	}
}

func TestCheckExecutable(t *testing.T) {
	self, err := os.Executable()
	if err != nil {
		t.Fatal(err)
	}
	dir := t.TempDir()
	// A copy of the test binary, named like a release asset.
	copied := filepath.Join(dir, "wa-transcriber-v9.9.9")
	if err := copyFile(self, copied, 0o755); err != nil {
		t.Fatal(err)
	}
	notExec := filepath.Join(dir, "text")
	if err := os.WriteFile(notExec, []byte("#!/bin/sh\necho hello\n"), 0o755); err != nil {
		t.Fatal(err)
	}
	noBit := filepath.Join(dir, "nobit")
	if err := os.WriteFile(noBit, []byte("not a binary"), 0o644); err != nil {
		t.Fatal(err)
	}
	empty := filepath.Join(dir, "empty")
	if err := os.WriteFile(empty, nil, 0o755); err != nil {
		t.Fatal(err)
	}
	truncated := filepath.Join(dir, "truncated")
	full, err := os.ReadFile(copied)
	if err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(truncated, full[:len(full)/2], 0o755); err != nil {
		t.Fatal(err)
	}

	tests := []struct {
		name string
		path string
		key  string
		ok   bool
	}{
		{"test binary", self, ownKey(t), true},
		{"copied binary", copied, ownKey(t), true},
		{"text file", notExec, ownKey(t), false},
		{"not executable bit", noBit, ownKey(t), false},
		{"empty file", empty, ownKey(t), false},
		{"truncated ELF", truncated, ownKey(t), false},
		{"missing file", filepath.Join(dir, "nope"), ownKey(t), false},
		{"foreign asset key", copied, otherKey(t), false},
		{"bad asset key", copied, "linux-pdp11", false},
		{"empty asset key", copied, "", false},
		{"not a linux key", copied, "darwin-amd64", false},
		{"traversal key", copied, "linux-../../x", false},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			err := CheckExecutable(tc.path, tc.key)
			if tc.ok && err != nil {
				t.Fatalf("CheckExecutable: %v", err)
			}
			if !tc.ok && err == nil {
				t.Fatal("CheckExecutable accepted a foreign or broken file")
			}
		})
	}
}
