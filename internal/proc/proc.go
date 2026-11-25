// Package proc owns the child processes of the program when the kernel runs
// it as init (see internal/boot).
//
// PID 1 is the only reaper on the machine: an exited child stays a zombie
// until wait4() is called for it, and nothing above us will ever call it. The
// zombies are not hypothetical — wpa_supplicant, hostapd and dnsmasq daemonise
// by forking, so the process that ends up as our child is not the one we
// started and nobody waits for its exit.
//
// The obvious cure, a goroutine calling wait4(-1), is worse than the disease:
// it steals the exit status of the children os/exec is waiting for, and their
// Wait then fails with "waitid: no child processes" (measured on Go 1.26: two
// of two hundred cmd.Run() calls lost their status). So every pid the program
// starts is recorded here, and the sweep only waits for the ones that are not
// recorded — a pid we never started can only have been reparented to us.
package proc

import (
	"bytes"
	"context"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"strconv"
	"strings"
	"sync"
	"syscall"
	"time"
)

// Table is the set of child processes the program started itself.
type Table struct {
	mu      sync.Mutex
	started map[int]struct{}
	logf    func(string, ...any)
}

// New returns an empty table. logf may be nil; it receives one line per
// child that was reaped without its own waiter.
func New(logf func(string, ...any)) *Table {
	if logf == nil {
		logf = func(string, ...any) {}
	}
	return &Table{started: make(map[int]struct{}), logf: logf}
}

// defaultTable is what the package-level helpers use.
var defaultTable = New(nil)

// Default is the table the package-level helpers use.
func Default() *Table { return defaultTable }

// SetDefault replaces it (main installs a table with a logger).
func SetDefault(t *Table) {
	if t != nil {
		defaultTable = t
	}
}

// Start runs cmd and records the child, so the sweep leaves it alone.
func (t *Table) Start(cmd *exec.Cmd) error {
	if err := cmd.Start(); err != nil {
		return err
	}
	t.track(cmd.Process.Pid)
	return nil
}

// Wait waits for a command started with Start and forgets it.
//
// Every process the program starts has to be waited for through here: once
// the sweep has reaped a pid, that pid has no status left to report.
func (t *Table) Wait(cmd *exec.Cmd) error {
	err := cmd.Wait()
	if cmd.Process != nil {
		t.forget(cmd.Process.Pid)
	}
	return err
}

// Run starts cmd, waits for it and returns its error.
func (t *Table) Run(cmd *exec.Cmd) error {
	if err := t.Start(cmd); err != nil {
		return err
	}
	return t.Wait(cmd)
}

// CombinedOutput runs cmd and returns what it wrote on stdout and stderr,
// like (*exec.Cmd).CombinedOutput.
func (t *Table) CombinedOutput(cmd *exec.Cmd) ([]byte, error) {
	var buf bytes.Buffer
	cmd.Stdout = &buf
	cmd.Stderr = &buf
	err := t.Run(cmd)
	return buf.Bytes(), err
}

func (t *Table) track(pid int) {
	t.mu.Lock()
	t.started[pid] = struct{}{}
	t.mu.Unlock()
}

func (t *Table) forget(pid int) {
	t.mu.Lock()
	delete(t.started, pid)
	t.mu.Unlock()
}

func (t *Table) tracked(pid int) bool {
	t.mu.Lock()
	defer t.mu.Unlock()
	_, ok := t.started[pid]
	return ok
}

// Count is the number of recorded children that have not been waited for.
// It is what a test checks; the value is otherwise only a symptom.
func (t *Table) Count() int {
	t.mu.Lock()
	defer t.mu.Unlock()
	return len(t.started)
}

// Reap waits for the children that exited without anyone waiting for them
// and returns how many it took. It reads the process table from procDir and
// only touches zombies whose parent is this process and that it did not
// start.
//
// A missing or unreadable procDir is not an error: without /proc there is
// nothing to sweep, which is the case on a development machine and is
// harmless there.
func (t *Table) Reap(procDir string) int {
	ents, err := os.ReadDir(procDir)
	if err != nil {
		return 0
	}
	self := os.Getpid()
	reaped := 0
	for _, e := range ents {
		pid, err := strconv.Atoi(e.Name())
		if err != nil {
			continue
		}
		state, ppid, err := readStat(filepath.Join(procDir, e.Name(), "stat"))
		if err != nil || ppid != self || state != 'Z' {
			continue
		}
		// Started by us: os/exec owns the status, and taking it here would
		// make that Wait fail.
		if t.tracked(pid) {
			continue
		}
		var ws syscall.WaitStatus
		got, err := syscall.Wait4(pid, &ws, syscall.WNOHANG, nil)
		if err != nil || got != pid {
			continue
		}
		reaped++
		t.logf("reaped a child that nobody waited for", "pid", pid, "exit", ws.ExitStatus())
	}
	return reaped
}

// ReapLoop sweeps every interval until ctx is done. A nil or negative
// interval means one second.
func (t *Table) ReapLoop(ctx context.Context, procDir string, interval time.Duration) {
	if interval <= 0 {
		interval = time.Second
	}
	tk := time.NewTicker(interval)
	defer tk.Stop()
	for {
		select {
		case <-ctx.Done():
			return
		case <-tk.C:
			t.Reap(procDir)
		}
	}
}

// readStat returns the state and the parent pid from a /proc/<pid>/stat
// body. The second field is the command name in parentheses, which may
// contain spaces and even ')', so the remaining fields are counted from the
// last ')'.
func readStat(path string) (byte, int, error) {
	b, err := os.ReadFile(path)
	if err != nil {
		return 0, 0, err
	}
	s := string(b)
	i := strings.LastIndexByte(s, ')')
	if i < 0 || i+2 >= len(s) {
		return 0, 0, fmt.Errorf("proc: malformed stat %q", s)
	}
	fields := strings.Fields(s[i+1:])
	if len(fields) < 2 {
		return 0, 0, fmt.Errorf("proc: malformed stat %q", s)
	}
	if len(fields[0]) != 1 {
		return 0, 0, fmt.Errorf("proc: malformed state %q", fields[0])
	}
	ppid, err := strconv.Atoi(fields[1])
	if err != nil {
		return 0, 0, fmt.Errorf("proc: malformed ppid %q", fields[1])
	}
	return fields[0][0], ppid, nil
}

// Package-level helpers for the callers that do not need a table of their own.
// They use the default table, which main replaces with a logging one.

// Start runs cmd and records the child in the default table.
func Start(cmd *exec.Cmd) error { return defaultTable.Start(cmd) }

// Wait waits for cmd in the default table.
func Wait(cmd *exec.Cmd) error { return defaultTable.Wait(cmd) }

// Run starts and waits for cmd in the default table.
func Run(cmd *exec.Cmd) error { return defaultTable.Run(cmd) }

// CombinedOutput is (*exec.Cmd).CombinedOutput through the default table.
func CombinedOutput(cmd *exec.Cmd) ([]byte, error) { return defaultTable.CombinedOutput(cmd) }
