// Package logbuf keeps the most recent log output in RAM (never on the SD
// card) for the web UI, while also copying it to another writer (stderr).
package logbuf

import (
	"bufio"
	"io"
	"os"
	"strconv"
	"strings"
	"sync"
)

// Buffer is a byte-bounded ring of log lines. It implements io.Writer; each
// Write may contain one or more newline-terminated lines.
type Buffer struct {
	mu    sync.Mutex
	max   int
	size  int
	lines []string // oldest first
	tee   io.Writer
}

// New returns a buffer holding at most maxBytes of log lines, copying every
// write to tee (may be nil).
func New(maxBytes int, tee io.Writer) *Buffer {
	return &Buffer{max: maxBytes, tee: tee}
}

func (b *Buffer) Write(p []byte) (int, error) {
	if b.tee != nil {
		_, _ = b.tee.Write(p)
	}
	b.mu.Lock()
	defer b.mu.Unlock()
	for line := range strings.Lines(string(p)) {
		line = strings.TrimRight(line, "\n")
		if len(line) > b.max {
			line = line[:b.max]
		}
		b.lines = append(b.lines, line)
		b.size += len(line)
		drop := 0
		for b.size > b.max {
			b.size -= len(b.lines[drop])
			drop++
		}
		if drop > 0 {
			// Copy instead of reslicing so the backing array does not grow forever.
			b.lines = append(b.lines[:0:0], b.lines[drop:]...)
		}
	}
	return len(p), nil
}

// Lines returns a copy of the buffered lines, oldest first.
func (b *Buffer) Lines() []string {
	b.mu.Lock()
	defer b.mu.Unlock()
	return append([]string(nil), b.lines...)
}

// SizeForMemory picks a buffer size of ~1% of total RAM, clamped to
// 256 KB–4 MB. memTotal is in bytes; 0 means unknown.
func SizeForMemory(memTotal uint64) int {
	const lo, hi = 256 << 10, 4 << 20
	n := memTotal / 100
	switch {
	case n < lo:
		return lo
	case n > hi:
		return hi
	}
	return int(n)
}

// MemTotal reads MemTotal from /proc/meminfo (0 if unavailable).
func MemTotal() uint64 {
	f, err := os.Open("/proc/meminfo")
	if err != nil {
		return 0
	}
	defer f.Close()
	return parseMemTotal(f)
}

func parseMemTotal(r io.Reader) uint64 {
	sc := bufio.NewScanner(r)
	for sc.Scan() {
		fields := strings.Fields(sc.Text())
		if len(fields) >= 2 && fields[0] == "MemTotal:" {
			kb, err := strconv.ParseUint(fields[1], 10, 64)
			if err != nil {
				return 0
			}
			return kb * 1024
		}
	}
	return 0
}
