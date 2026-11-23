package main

import (
	"bytes"
	"io"
	"os"
	"os/exec"
	"strings"
	"sync"
	"syscall"
	"testing"
	"time"
	"unsafe"
)

// TestPtyEndToEnd drives the real program against a real shell on a
// pseudo-terminal pair: the tool holds the master (what the host's ttyACM0
// would be) and `sh` runs on the slave (what the board's getty shell would
// be). Nothing is faked except the wire.
//
// The timing assertion is the regression the shell script had: it decided an
// answer was complete by reading until the line went quiet, so an instant
// command still cost seconds. Here each call must finish in a fraction of a
// second, and the whole subtest budget is 2 s.
//
// A plain `sh -i` is already at a prompt, so this covers the frame, the
// markers, the streaming and the status; the login dance itself is covered by
// console_test.go and login_test.go through an injected io.ReadWriter, which
// is the fallback when a platform has no pty at all.
func TestPtyEndToEnd(t *testing.T) {
	// Probe once: without a pty every subtest would skip for the same
	// reason, so say it once and clearly.
	probeM, probeS, err := ptyPair()
	if err != nil {
		t.Skipf("no pseudo-terminal on this machine, so the end-to-end test is skipped "+
			"(the same paths are covered through an injected io.ReadWriter in console_test.go): %v", err)
	}
	probeM.Close()
	probeS.Close()

	tests := []struct {
		name     string
		cmd      string
		wantCode int
		wantOut  string
		notOut   string
	}{
		{name: "instant command", cmd: "echo hi", wantCode: 0, wantOut: "hi\n", notOut: "__SRUN_"},
		{name: "non-zero status", cmd: "sh -c 'exit 3'", wantCode: 3, notOut: "__SRUN_"},
		{name: "status passes through", cmd: "echo done; sh -c 'exit 42'", wantCode: 42, wantOut: "done\n"},
		{name: "both streams", cmd: "echo out; echo err >&2", wantCode: 0, wantOut: "out\nerr\n"},
		{name: "shell builtins and pipes", cmd: "printf 'a\\nb\\n' | grep b", wantCode: 0, wantOut: "b\n"},
		// A command that prints something shaped like the END tag must not
		// end the run early: only the nonce makes a line the real tag.
		{name: "fake end tag is just output", cmd: "echo __SRUN_E__:0", wantCode: 0, wantOut: "__SRUN_E__:0\n"},
		{name: "fake begin tag is just output", cmd: "echo __SRUN_B__", wantCode: 0, wantOut: "__SRUN_B__\n"},
		// A '#' would comment out the closing brace and the END tag.
		{name: "hash in the command", cmd: "echo a # b", wantCode: 0, wantOut: "a\n"},
		// The command must not run in a subshell: cd has to stick.
		{name: "cd takes effect", cmd: "cd /tmp; pwd", wantCode: 0, wantOut: "/tmp\n"},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			master, slave, err := ptyPair()
			if err != nil {
				t.Skipf("no pty: %v", err)
			}
			defer master.Close()
			defer slave.Close()

			sh := exec.Command("sh", "-i")
			sh.Stdin, sh.Stdout, sh.Stderr = slave, slave, slave
			// PS1 is what the tool looks for as the shell prompt, exactly
			// like the board's getty sets "# " for root.
			sh.Env = []string{"PS1=# ", "ENV=", "TERM=dumb", "PATH=" + os.Getenv("PATH")}
			if err := sh.Start(); err != nil {
				t.Skipf("cannot start sh on a pty: %v", err)
			}
			var once sync.Once
			stopShell := func() {
				once.Do(func() {
					_ = sh.Process.Kill()
					_ = sh.Wait()
				})
			}
			defer stopShell()

			// Keep the slave open in the test as well, so the tool's reads
			// see the shell's output even as the shell's process table is
			// torn down.
			var stdout, stderr lockedBuffer
			start := time.Now()
			p := startOn(t, master)
			code, err := converse(p, sessionOptions{
				dev:    "pty",
				user:   "root",
				idle:   10 * time.Second,
				prompt: 5 * time.Second,
				cmd:    tc.cmd,
			}, &stdout, &stderr)
			elapsed := time.Since(start)
			// The reader is left to notice the stop on its own in the tool;
			// the test must join it before closing the descriptor.
			p.wait(time.Second)

			if err != nil {
				t.Fatalf("session failed after %s: %v\nstdout:\n%s\nstderr:\n%s",
					elapsed, err, stdout.String(), stderr.String())
			}
			if code != tc.wantCode {
				t.Errorf("exit status = %d, want %d\nstdout:\n%s\nstderr:\n%s",
					code, tc.wantCode, stdout.String(), stderr.String())
			}
			out := stdout.String()
			if tc.wantOut != "" && !strings.Contains(out, tc.wantOut) {
				t.Errorf("stdout = %q, want it to contain %q", out, tc.wantOut)
			}
			if tc.notOut != "" && strings.Contains(out, tc.notOut) {
				t.Errorf("stdout = %q, must not contain the marker %q", out, tc.notOut)
			}
			// The shell script's quiet period cost seconds; anything near
			// that on an identical command is the regression coming back.
			if elapsed > 2*time.Second {
				t.Errorf("the call took %s, which is the quiet-period behaviour again", elapsed)
			}
			t.Logf("%q -> %d in %s", tc.cmd, code, elapsed)
		})
	}
}

// TestPtyStreamsWhileTheCommandRuns checks the streaming promise on real
// hardware-free plumbing: the first half of the output is visible before the
// command finishes. The shell writes "one", sleeps, then "two"; the tool must
// have handed "one" to stdout while the sleep is still running.
func TestPtyStreamsWhileTheCommandRuns(t *testing.T) {
	master, slave, err := ptyPair()
	if err != nil {
		t.Skipf("no pty on this machine: %v", err)
	}
	defer master.Close()
	defer slave.Close()

	sh := exec.Command("sh", "-i")
	sh.Stdin, sh.Stdout, sh.Stderr = slave, slave, slave
	sh.Env = []string{"PS1=# ", "ENV=", "TERM=dumb", "PATH=" + os.Getenv("PATH")}
	if err := sh.Start(); err != nil {
		t.Skipf("cannot start sh on a pty: %v", err)
	}
	defer func() {
		_ = sh.Process.Kill()
		_ = sh.Wait()
	}()

	var stdout, stderr lockedBuffer
	p := startOn(t, master)
	defer p.wait(time.Second)
	done := make(chan struct{})
	var code int
	var serr error
	go func() {
		code, serr = converse(p, sessionOptions{
			dev: "pty", user: "root", idle: 10 * time.Second, prompt: 5 * time.Second,
			cmd: "echo one; sleep 0.4; echo two",
		}, &stdout, &stderr)
		close(done)
	}()

	seenEarly := false
	deadline := time.Now().Add(2 * time.Second)
	for time.Now().Before(deadline) {
		if strings.Contains(stdout.String(), "one\n") {
			seenEarly = !strings.Contains(stdout.String(), "two\n")
			break
		}
		select {
		case <-done:
			t.Fatalf("the session finished before the first line was streamed: %q", stdout.String())
		case <-time.After(2 * time.Millisecond):
		}
	}
	<-done
	if serr != nil {
		t.Fatalf("session failed: %v", serr)
	}
	if code != 0 {
		t.Fatalf("exit status = %d, want 0", code)
	}
	if !seenEarly {
		t.Errorf("the first line was not visible while the command was still running: %q", stdout.String())
	}
	if got := stdout.String(); got != "one\ntwo\n" {
		t.Errorf("stdout = %q, want %q", got, "one\ntwo\n")
	}
}

// TestPtyLoginDanceOnAGetty stands in for a board whose getty is waiting: a
// small Go program (this test binary, re-run) plays getty -- it prints the
// login prompt, reads the name, asks for the secret and then hands the line
// to a shell. It checks the whole dance over a real tty, including that "\r"
// (not "\n") is what wakes the reader, as on the board.
func TestPtyLoginDanceOnAGetty(t *testing.T) {
	master, slave, err := ptyPair()
	if err != nil {
		t.Skipf("no pty on this machine: %v", err)
	}
	defer master.Close()
	defer slave.Close()

	getty := exec.Command(os.Args[0])
	getty.Env = append(os.Environ(), "SERIALRUN_FAKE_GETTY=1")
	getty.Stdin, getty.Stdout, getty.Stderr = slave, slave, slave
	if err := getty.Start(); err != nil {
		t.Skipf("cannot start the fake getty on a pty: %v", err)
	}
	defer func() {
		_ = getty.Process.Kill()
		_ = getty.Wait()
	}()

	var stdout, stderr lockedBuffer
	start := time.Now()
	p := startOn(t, master)
	defer p.wait(time.Second)
	code, err := converse(p, sessionOptions{
		dev: "pty", user: "root", idle: 10 * time.Second, prompt: 5 * time.Second,
		cmd: "echo hello from the board",
	}, &stdout, &stderr)
	if err != nil {
		t.Fatalf("session failed after %s: %v\nstdout:\n%s\nstderr:\n%s",
			time.Since(start), err, stdout.String(), stderr.String())
	}
	if code != 0 {
		t.Fatalf("exit status = %d, want 0\nstdout:\n%s\nstderr:\n%s", code, stdout.String(), stderr.String())
	}
	if !strings.Contains(stdout.String(), "hello from the board") {
		t.Fatalf("stdout = %q, want the command's output", stdout.String())
	}
	if !strings.Contains(stderr.String(), `(sent "root")`) {
		t.Errorf("stderr should show the login dance, got:\n%s", stderr.String())
	}
	if time.Since(start) > 2*time.Second {
		t.Errorf("the login dance and the command took %s", time.Since(start))
	}
}

// TestPtyIgnoresConsoleChatter proves the tool is event-driven on a real tty:
// a command that prints nothing for a moment and then only its marker must
// still return at once, with no answer being swallowed. The shell sleep is
// what the old script measured as "quiet".
func TestPtyIgnoresConsoleChatter(t *testing.T) {
	master, slave, err := ptyPair()
	if err != nil {
		t.Skipf("no pty on this machine: %v", err)
	}
	defer master.Close()
	defer slave.Close()

	sh := exec.Command("sh", "-i")
	sh.Stdin, sh.Stdout, sh.Stderr = slave, slave, slave
	sh.Env = []string{"PS1=# ", "ENV=", "TERM=dumb", "PATH=" + os.Getenv("PATH")}
	if err := sh.Start(); err != nil {
		t.Skipf("cannot start sh on a pty: %v", err)
	}
	defer func() {
		_ = sh.Process.Kill()
		_ = sh.Wait()
	}()

	var stdout, stderr lockedBuffer
	start := time.Now()
	p := startOn(t, master)
	defer p.wait(time.Second)
	code, err := converse(p, sessionOptions{
		dev: "pty", user: "root", idle: 10 * time.Second, prompt: 5 * time.Second,
		// The kernel-message lookalikes and the silence in the middle are
		// what a shared console looks like.
		cmd: "echo '[  12.3] usb 1-1: new high-speed USB device'; sleep 0.2; echo done",
	}, &stdout, &stderr)
	if err != nil {
		t.Fatalf("session failed: %v", err)
	}
	if code != 0 {
		t.Fatalf("exit status = %d, want 0", code)
	}
	if !strings.Contains(stdout.String(), "new high-speed USB device") ||
		!strings.Contains(stdout.String(), "done") {
		t.Fatalf("stdout = %q, both lines of the command must arrive", stdout.String())
	}
	if time.Since(start) > 2*time.Second {
		t.Errorf("the call took %s", time.Since(start))
	}
}

// TestMain doubles as the fake getty when the test binary is re-run with
// SERIALRUN_FAKE_GETTY set, which is how the login dance gets a real tty
// without a second program to install.
func TestMain(m *testing.M) {
	if os.Getenv("SERIALRUN_FAKE_GETTY") != "" {
		fakeGetty()
		return
	}
	os.Exit(m.Run())
}

// fakeGetty is a board whose getty is waiting: print the prompt, take the
// name, ask for the secret (empty here) and then hand over a shell.
func fakeGetty() {
	tty, err := os.OpenFile("/dev/stdin", os.O_RDWR, 0)
	if err != nil {
		return
	}
	// A getty reads a raw line: the board's console delivers \r as \r, and
	// a canonical stdin would wait for a \n that the tool never sends. The
	// shell started below gets the terminal back in its normal cooked mode,
	// which is what a real getty does before handing over.
	raw, err := makeRawBlocking(tty)
	if err != nil {
		return
	}
	// getty prints "login:" and waits for a \r; the board's tty echoes what
	// we type. The prompts end their line so the tool can recognise them,
	// which is what a getty does once it echoes the entered name.
	io.WriteString(tty, "beaglebone login:\r\n")
	readLine(tty)
	io.WriteString(tty, "Password:\r\n")
	readLine(tty)
	io.WriteString(tty, "\r\n")
	raw()
	sh := exec.Command("sh", "-i")
	sh.Stdin, sh.Stdout, sh.Stderr = tty, tty, tty
	sh.Env = []string{"PS1=# ", "ENV=", "TERM=dumb", "PATH=" + os.Getenv("PATH")}
	_ = sh.Run()
}

// readLine consumes one "\r"-terminated line, like getty does: a bare "\r"
// ends it, which is the reason the tool sends "\r" and not "\n". Everything
// up to and including that "\r" is taken, so the shell that follows starts
// with a clean input queue -- a getty that left the terminator behind would
// hand the shell an empty command line.
func readLine(f *os.File) {
	b := make([]byte, 1)
	for {
		// A raw fd returns 0 bytes while nothing is pending, which is not
		// the end of the line: wait for a byte before deciding.
		if _, err := waitReadableAny(f, time.Minute); err != nil {
			return
		}
		n, err := f.Read(b)
		if err != nil {
			return
		}
		if n == 0 {
			continue
		}
		if b[0] == '\r' || b[0] == '\n' {
			return
		}
	}
}

// makeRawBlocking is raw mode for a reader that wants to wait: everything the
// tool turns off, except that a read blocks until a byte arrives (VMIN=1).
func makeRawBlocking(f *os.File) (func(), error) {
	var old syscall.Termios
	if err := ioctl(f, syscall.TCGETS, unsafe.Pointer(&old)); err != nil {
		return nil, err
	}
	if err := setRaw(f); err != nil {
		return nil, err
	}
	return func() { _ = ioctl(f, syscall.TCSETS, unsafe.Pointer(&old)) }, nil
}

// startOn opens the port the way runOn does -- raw mode is not needed on a
// pty master and is skipped there -- and starts its reader.
func startOn(t *testing.T, f *os.File) *port {
	t.Helper()
	if restore, err := prepareHost(f); err == nil {
		t.Cleanup(restore)
	}
	// Short deadlines: the reader only notices a stop on its next wake-up,
	// and the test joins it (p.wait) before closing the descriptor.
	p := newPort(f, time.Second, 5*time.Second)
	p.start()
	return p
}

// lockedBuffer is a bytes.Buffer safe between the session goroutine and the
// test.
type lockedBuffer struct {
	mu  sync.Mutex
	buf bytes.Buffer
}

func (b *lockedBuffer) Write(p []byte) (int, error) {
	b.mu.Lock()
	defer b.mu.Unlock()
	return b.buf.Write(p)
}

func (b *lockedBuffer) String() string {
	b.mu.Lock()
	defer b.mu.Unlock()
	return b.buf.String()
}
