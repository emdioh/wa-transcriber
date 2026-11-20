package main

import (
	"os"
	"testing"
)

func TestCheckWritable(t *testing.T) {
	dir := t.TempDir()
	if err := checkWritable(dir); err != nil {
		t.Fatalf("writable dir: %v", err)
	}
	entries, err := os.ReadDir(dir)
	if err != nil {
		t.Fatal(err)
	}
	if len(entries) != 0 {
		t.Fatalf("probe file left behind: %v", entries)
	}
}

func TestCheckWritableRejectsReadOnlyDir(t *testing.T) {
	if os.Geteuid() == 0 {
		t.Skip("root bypasses directory permissions")
	}
	dir := t.TempDir()
	if err := os.Chmod(dir, 0o500); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = os.Chmod(dir, 0o700) })
	if err := checkWritable(dir); err == nil {
		t.Fatal("read-only directory accepted")
	}
	if err := dataDirError(dir, os.ErrPermission); err == nil {
		t.Fatal("no error built")
	}
}
