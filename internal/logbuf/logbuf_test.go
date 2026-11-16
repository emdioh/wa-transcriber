package logbuf

import (
	"bytes"
	"strings"
	"testing"
)

func TestRingDropsOldest(t *testing.T) {
	var tee bytes.Buffer
	b := New(10, &tee)
	b.Write([]byte("aaaa\nbbbb\n"))
	b.Write([]byte("cccc\n"))
	got := b.Lines()
	if strings.Join(got, ",") != "bbbb,cccc" {
		t.Fatalf("lines = %v", got)
	}
	if tee.String() != "aaaa\nbbbb\ncccc\n" {
		t.Fatalf("tee = %q", tee.String())
	}
}

func TestLongLineTruncated(t *testing.T) {
	b := New(4, nil)
	b.Write([]byte("123456789\n"))
	if got := b.Lines(); len(got) != 1 || got[0] != "1234" {
		t.Fatalf("lines = %v", got)
	}
}

func TestSizeForMemory(t *testing.T) {
	cases := map[uint64]int{
		0:         256 << 10,
		16 << 20:  256 << 10, // 1% of 16 MB is below the floor
		128 << 20: 1342177,
		256 << 20: 2684354,
		8 << 30:   4 << 20,
	}
	for mem, want := range cases {
		if got := SizeForMemory(mem); got != want {
			t.Errorf("SizeForMemory(%d) = %d, want %d", mem, got, want)
		}
	}
}

func TestParseMemTotal(t *testing.T) {
	in := "MemTotal:         949580 kB\nMemFree:  1 kB\n"
	if got := parseMemTotal(strings.NewReader(in)); got != 949580*1024 {
		t.Fatalf("got %d", got)
	}
}
