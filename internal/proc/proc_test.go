package proc

import (
	"context"
	"errors"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"strconv"
	"strings"
	"sync"
	"testing"
	"time"
)

func TestReadStat(t *testing.T) {
	tests := []struct {
		name  string
		body  string
		state byte
		ppid  int
		fail  bool
	}{
		{"simple", "1234 (bash) S 1 1234 1234 0 -1 4194304 1 0 0 0", 'S', 1, false},
		{"space in comm", "7 (wa transcriber) Z 42 7 7 0 -1 1 1 0", 'Z', 42, false},
		{"paren in comm", "8 (a) b) R 9 8 8 0 -1 1 1 0", 'R', 9, false},
		{"truncated", "8 (bash) ", 0, 0, true},
		{"no comm", "8 bash S 1", 0, 0, true},
		{"bad ppid", "8 (bash) S x", 0, 0, true},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			p := filepath.Join(t.TempDir(), "stat")
			if err := os.WriteFile(p, []byte(tc.body), 0o600); err != nil {
				t.Fatal(err)
			}
			state, ppid, err := readStat(p)
			if tc.fail {
				if err == nil {
					t.Fatalf("readStat(%q) = %c, %d, nil; want an error", tc.body, state, ppid)
				}
				return
			}
			if err != nil {
				t.Fatalf("readStat: %v", err)
			}
			if state != tc.state || ppid != tc.ppid {
				t.Fatalf("readStat = %c, %d; want %c, %d", state, ppid, tc.state, tc.ppid)
			}
		})
	}
}

// TestSweepLeavesStartedChildrenAlone is the property the whole package
// exists for: a child of ours that exited is a zombie whose parent is us, and
// the sweep must not take it, or its Wait would fail with "no child
// processes".
func TestSweepLeavesStartedChildrenAlone(t *testing.T) {
	tbl := New(nil)
	cmd := exec.Command("/bin/sh", "-c", "exit 7")
	if err := tbl.Start(cmd); err != nil {
		t.Fatalf("Start: %v", err)
	}
	pid := cmd.Process.Pid
	waitZombie(t, pid)

	if n := tbl.Reap("/proc"); n != 0 {
		t.Fatalf("Reap took %d children it started", n)
	}
	err := tbl.Wait(cmd)
	var exit *exec.ExitError
	if !errors.As(err, &exit) || exit.ExitCode() != 7 {
		t.Fatalf("Wait = %v, want exit status 7", err)
	}
	if tbl.Count() != 0 {
		t.Fatalf("table still holds %d pids after Wait", tbl.Count())
	}
}

// TestSweepReapsUntrackedChild is the case the sweep is for: a child nobody
// is waiting for (on a board: a daemonised wpa_supplicant) would stay a
// zombie for the lifetime of the machine.
func TestSweepReapsUntrackedChild(t *testing.T) {
	tbl := New(nil)
	cmd := exec.Command("/bin/sh", "-c", "exit 0")
	if err := cmd.Start(); err != nil { // deliberately not through the table
		t.Fatalf("Start: %v", err)
	}
	pid := cmd.Process.Pid
	defer func() { _ = cmd.Wait() }() // no-op if the sweep got there first
	waitZombie(t, pid)

	if n := tbl.Reap("/proc"); n != 1 {
		t.Fatalf("Reap = %d, want 1", n)
	}
	if _, err := os.Stat(filepath.Join("/proc", itoa(pid))); !os.IsNotExist(err) {
		t.Fatalf("pid %d is still in the process table (err=%v)", pid, err)
	}
}

// TestSweepIgnoresLiveAndForeignPids: only zombies of this process are ours
// to take. PID 1 belongs to the machine, and wait4 on it answers ECHILD.
func TestSweepIgnoresLiveAndForeignPids(t *testing.T) {
	tbl := New(nil)
	live := exec.Command("/bin/sh", "-c", "sleep 30")
	if err := tbl.Start(live); err != nil {
		t.Fatalf("Start: %v", err)
	}
	defer func() {
		_ = live.Process.Kill()
		_ = tbl.Wait(live)
	}()
	if n := tbl.Reap("/proc"); n != 0 {
		t.Fatalf("Reap took %d live children", n)
	}

	dir := t.TempDir()
	mkStat(t, dir, 1, "S", 1) // a live, foreign process
	mkStat(t, dir, 4242, "Z", 999)
	if n := tbl.Reap(dir); n != 0 {
		t.Fatalf("Reap took %d processes that are not our children", n)
	}
}

func TestSweepWithNoProc(t *testing.T) {
	tbl := New(nil)
	if n := tbl.Reap(filepath.Join(t.TempDir(), "missing")); n != 0 {
		t.Fatalf("Reap on a missing /proc = %d", n)
	}
}

func TestReapLoopStopsWithContext(t *testing.T) {
	tbl := New(nil)
	ctx, cancel := context.WithCancel(context.Background())
	done := make(chan struct{})
	go func() {
		tbl.ReapLoop(ctx, "/proc", time.Millisecond)
		close(done)
	}()
	cancel()
	select {
	case <-done:
	case <-time.After(2 * time.Second):
		t.Fatal("ReapLoop did not return after the context was cancelled")
	}
}

func TestRunReportsExitStatus(t *testing.T) {
	err := Run(exec.Command("/bin/sh", "-c", "exit 3"))
	var exit *exec.ExitError
	if !errors.As(err, &exit) || exit.ExitCode() != 3 {
		t.Fatalf("Run = %v, want exit status 3", err)
	}
	out, err := CombinedOutput(exec.Command("/bin/sh", "-c", "echo hi"))
	if err != nil || string(out) != "hi\n" {
		t.Fatalf("CombinedOutput = %q, %v", out, err)
	}
}

// TestSetLoggerPreservesTracked mirrors init mode: boot.Setup starts console
// gettys on the default table before main attaches its slog logger. Attaching
// the logger must not drop already-tracked pids.
func TestSetLoggerPreservesTracked(t *testing.T) {
	prev := Default()
	SetDefault(New(nil))
	t.Cleanup(func() { SetDefault(prev) })

	cmd := exec.Command("/bin/sh", "-c", "exit 9")
	if err := Start(cmd); err != nil {
		t.Fatalf("Start: %v", err)
	}
	pid := cmd.Process.Pid
	waitZombie(t, pid)

	var mu sync.Mutex
	var lines []string
	SetLogger(func(format string, args ...any) {
		mu.Lock()
		lines = append(lines, fmt.Sprintf(format, args...))
		mu.Unlock()
	})

	if n := Default().Reap("/proc"); n != 0 {
		t.Fatalf("Reap after SetLogger took %d tracked children", n)
	}
	err := Wait(cmd)
	var exit *exec.ExitError
	if !errors.As(err, &exit) || exit.ExitCode() != 9 {
		t.Fatalf("Wait = %v, want exit status 9", err)
	}
	if Default().Count() != 0 {
		t.Fatalf("default table still holds %d pids after Wait", Default().Count())
	}
	mu.Lock()
	gotLines := append([]string(nil), lines...)
	mu.Unlock()
	if len(gotLines) != 0 {
		t.Fatalf("unexpected log lines for tracked child: %v", gotLines)
	}
}

func TestConcurrentStartWaitReapSetLogger(t *testing.T) {
	tbl := New(nil)
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()

	var bg sync.WaitGroup
	bg.Add(2)
	go func() {
		defer bg.Done()
		for ctx.Err() == nil {
			tbl.SetLogger(func(format string, args ...any) {
				_ = fmt.Sprintf(format, args...)
			})
			tbl.SetLogger(nil)
		}
	}()
	go func() {
		defer bg.Done()
		for ctx.Err() == nil {
			tbl.Reap("/proc")
		}
	}()

	const workers = 4
	const perWorker = 10
	var wg sync.WaitGroup
	errs := make(chan error, workers*perWorker)
	for range workers {
		wg.Add(1)
		go func() {
			defer wg.Done()
			for range perWorker {
				cmd := exec.Command("/bin/sh", "-c", "exit 5")
				if err := tbl.Start(cmd); err != nil {
					errs <- fmt.Errorf("Start: %w", err)
					return
				}
				err := tbl.Wait(cmd)
				var exit *exec.ExitError
				if !errors.As(err, &exit) || exit.ExitCode() != 5 {
					errs <- fmt.Errorf("Wait = %v, want exit status 5", err)
					return
				}
			}
		}()
	}
	wg.Wait()
	cancel()
	bg.Wait()
	close(errs)
	for err := range errs {
		t.Fatal(err)
	}
	if got := tbl.Count(); got != 0 {
		t.Fatalf("table still holds %d pids", got)
	}
}

func TestReapLogsFormattedMessage(t *testing.T) {
	tbl := New(nil)
	var mu sync.Mutex
	var lines []string
	tbl.SetLogger(func(format string, args ...any) {
		mu.Lock()
		lines = append(lines, fmt.Sprintf(format, args...))
		mu.Unlock()
	})

	cmd := exec.Command("/bin/sh", "-c", "exit 42")
	if err := cmd.Start(); err != nil { // deliberately untracked
		t.Fatalf("Start: %v", err)
	}
	pid := cmd.Process.Pid
	defer func() { _ = cmd.Wait() }()
	waitZombie(t, pid)

	if n := tbl.Reap("/proc"); n != 1 {
		t.Fatalf("Reap = %d, want 1", n)
	}

	mu.Lock()
	defer mu.Unlock()
	if len(lines) != 1 {
		t.Fatalf("got %d log lines, want 1: %v", len(lines), lines)
	}
	if strings.Contains(lines[0], "%!(") {
		t.Fatalf("log line has fmt formatting error: %q", lines[0])
	}
	want := fmt.Sprintf("reaped a child that nobody waited for pid=%d exit=42", pid)
	if lines[0] != want {
		t.Fatalf("log line = %q, want %q", lines[0], want)
	}
}

// waitZombie blocks until pid has exited and is a zombie (state Z), which is
// what the sweep looks for.
func waitZombie(t *testing.T, pid int) {
	t.Helper()
	deadline := time.Now().Add(5 * time.Second)
	for time.Now().Before(deadline) {
		state, _, err := readStat(filepath.Join("/proc", itoa(pid), "stat"))
		if err == nil && state == 'Z' {
			return
		}
		time.Sleep(time.Millisecond)
	}
	t.Fatalf("pid %d never became a zombie", pid)
}

func mkStat(t *testing.T, dir string, pid int, state string, ppid int) {
	t.Helper()
	d := filepath.Join(dir, itoa(pid))
	if err := os.MkdirAll(d, 0o700); err != nil {
		t.Fatal(err)
	}
	body := itoa(pid) + " (cmd) " + state + " " + itoa(ppid) + " 1 1 0 -1 1 1 0"
	if err := os.WriteFile(filepath.Join(d, "stat"), []byte(body), 0o600); err != nil {
		t.Fatal(err)
	}
}

func itoa(n int) string { return strconv.Itoa(n) }
