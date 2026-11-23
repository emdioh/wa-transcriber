package main

import (
	"bytes"
	"errors"
	"io"
	"strings"
	"sync"
	"testing"
	"time"
)

// fakeConsole runs the real session logic against an io.ReadWriter instead of
// a tty: everything read is replayed as console events, everything written is
// collected, and the test decides what "the board" says next. This is the
// injection point used when no pty is available (pty_test.go covers the same
// code against a real sh).
type fakeConsole struct {
	t     *testing.T
	c     *console
	port  *fakePort
	out   *syncBuffer // the command's output (written by the session goroutine)
	noise *syncBuffer // stderr side: login dialogue, echoes, warnings
}

// syncBuffer is a bytes.Buffer safe for the two goroutines a test uses: the
// session writes, the test reads.
type syncBuffer struct {
	mu  sync.Mutex
	buf bytes.Buffer
}

func (b *syncBuffer) Write(p []byte) (int, error) {
	b.mu.Lock()
	defer b.mu.Unlock()
	return b.buf.Write(p)
}

func (b *syncBuffer) String() string {
	b.mu.Lock()
	defer b.mu.Unlock()
	return b.buf.String()
}

func (b *syncBuffer) Len() int {
	b.mu.Lock()
	defer b.mu.Unlock()
	return b.buf.Len()
}

type fakePort struct {
	events chan event
	writes bytes.Buffer
	mu     sync.Mutex
}

// Read is never called by the console: it is driven by the events channel,
// which is what the real port's reader goroutine would push. It exists only so
// the fake satisfies io.ReadWriter like the tty does.
func (p *fakePort) Read([]byte) (int, error) { return 0, io.EOF }

func (p *fakePort) Write(b []byte) (int, error) {
	p.mu.Lock()
	defer p.mu.Unlock()
	p.writes.Write(b)
	return len(b), nil
}

func (p *fakePort) sent() string {
	p.mu.Lock()
	defer p.mu.Unlock()
	s := p.writes.String()
	p.writes.Reset()
	return s
}

func newFakeConsole(t *testing.T) *fakeConsole {
	f := &fakeConsole{
		t:     t,
		port:  &fakePort{events: make(chan event, 100)},
		out:   &syncBuffer{},
		noise: &syncBuffer{},
	}
	f.c = &console{
		rw:     f.port,
		events: f.port.events,
		out:    f.out,
		errw:   f.noise,
		idle:   time.Second,
		prompt: time.Second,
		dev:    "/dev/fake",
		markers: markers{
			begin: "__SRUN_B__n0nce",
			end:   "__SRUN_E__n0nce",
		},
	}
	return f
}

// say delivers lines to the session the way the port would: each complete
// line as evLine, and an evPartial for a remainder the board did not end.
func (f *fakeConsole) say(data string) {
	var l lines
	for _, line := range l.feed([]byte(data)) {
		f.port.events <- event{kind: evLine, line: line}
	}
	if rest, ok := l.flush(); ok {
		f.port.events <- event{kind: evPartial, line: rest}
	}
}

func (f *fakeConsole) event(kind eventKind) { f.port.events <- event{kind: kind} }

// settle waits until the session goroutine has consumed everything queued so
// far. The events channel is buffered, so "sent" means "the session is idle
// again", and the test can then look at what it wrote without a race.
func (f *fakeConsole) settle() {
	deadline := time.Now().Add(2 * time.Second)
	for len(f.port.events) > 0 {
		if time.Now().After(deadline) {
			f.t.Fatal("the session stopped consuming events")
		}
		time.Sleep(100 * time.Microsecond)
	}
}

// TestConsoleLoginThenCommand drives the whole session through the fake port:
// login prompt -> user name, password prompt -> empty line, shell prompt ->
// the command frame, output streamed, END tag -> status.
func TestConsoleLoginThenCommand(t *testing.T) {
	f := newFakeConsole(t)
	result := make(chan struct {
		code int
		err  error
	}, 1)
	go func() {
		if err := f.c.answerLogin(newLogin("root")); err != nil {
			result <- struct {
				code int
				err  error
			}{0, err}
			return
		}
		f.c.sawBegin = false
		code, err := f.c.run("echo hi")
		result <- struct {
			code int
			err  error
		}{code, err}
	}()

	f.say("beaglebone login:\r\r\n")
	f.settle()
	if got := f.port.sent(); got != "root\r" {
		t.Fatalf("after the login prompt the board got %q, want %q", got, "root\r")
	}
	f.say("root\r\r\nPassword:\r\r\n")
	f.settle()
	if got := f.port.sent(); got != "\r" {
		t.Fatalf("after the password prompt the board got %q, want a bare CR", got)
	}
	f.say("Last login: Mon Nov 23\r\r\nroot@beaglebone:~# ")
	f.settle()
	if got := f.port.sent(); got != f.c.markers.command("echo hi") {
		t.Fatalf("the frame sent was %q", got)
	}

	// The tty echoes the line; the shell then prints its own tags and the
	// answer between them.
	f.say("echo __SRUN_B__n0nce; { echo hi\r\r\n")
	f.say("__SRUN_B__n0nce\r\r\nhi\r\r\n__SRUN_E__n0nce:0\r\r\n")

	select {
	case r := <-result:
		if r.err != nil {
			t.Fatalf("session failed: %v", r.err)
		}
		if r.code != 0 {
			t.Fatalf("exit status = %d, want 0", r.code)
		}
	case <-time.After(2 * time.Second):
		t.Fatal("the session did not return after the END tag")
	}
	if got := f.out.String(); got != "hi\n" {
		t.Fatalf("stdout = %q, want %q (only the output, no tags, no echo)", got, "hi\n")
	}
}

// TestConsoleExitStatus: the status in the END tag becomes the result.
func TestConsoleExitStatus(t *testing.T) {
	for _, want := range []int{0, 1, 3, 127, 255} {
		f := newFakeConsole(t)
		if err := f.answerLoginAtPrompt(); err != nil {
			t.Fatal(err)
		}
		go func() {
			f.say("__SRUN_B__n0nce\r\r\n")
			f.say("__SRUN_E__n0nce:" + itoa(want) + "\r\r\n")
		}()
		code, err := f.c.run("false")
		if err != nil {
			t.Fatalf("status %d: unexpected error %v", want, err)
		}
		if code != want {
			t.Errorf("exit status = %d, want %d", code, want)
		}
	}
}

// TestConsoleFakeEndTagIsOutput is the collision case: the command prints
// something that looks like the END tag (without the nonce, or with a
// different one). It must be printed, and the real tag must still end the run.
func TestConsoleFakeEndTagIsOutput(t *testing.T) {
	f := newFakeConsole(t)
	if err := f.answerLoginAtPrompt(); err != nil {
		t.Fatal(err)
	}
	out := make(chan int, 1)
	go func() {
		code, _ := f.c.run("cat fake")
		out <- code
	}()
	f.say("__SRUN_B__n0nce\r\r\n")
	f.say("__SRUN_E__:0\r\r\n")        // no nonce
	f.say("__SRUN_E__deadbee:0\r\r\n") // wrong nonce
	f.say("__SRUN_E__n0nce\r\r\n")     // real tag, no status: mangled
	select {
	case code := <-out:
		t.Fatalf("the session ended on a fake tag with status %d", code)
	case <-time.After(50 * time.Millisecond):
	}
	f.say("still here\r\r\n__SRUN_E__n0nce:7\r\r\n")
	select {
	case code := <-out:
		if code != 7 {
			t.Fatalf("exit status = %d, want 7", code)
		}
	case <-time.After(2 * time.Second):
		t.Fatal("the real END tag did not end the run")
	}
	if !strings.Contains(f.out.String(), "__SRUN_E__:0") {
		t.Fatalf("the fake tag must be printed as output, stdout = %q", f.out.String())
	}
}

// TestConsoleNoPromptIdle: a console that says nothing produces a clear error
// that shows what it saw, and the error names the phase.
func TestConsoleNoPromptIdle(t *testing.T) {
	f := newFakeConsole(t)
	f.say("U-Boot 2022.04\r\r\n")
	done := make(chan error, 1)
	go func() { done <- f.c.answerLogin(newLogin("root")) }()
	time.Sleep(20 * time.Millisecond)
	f.event(evIdleTimeout)
	err := <-done
	var ce *consoleError
	if !errors.As(err, &ce) {
		t.Fatalf("got %v, want a consoleError", err)
	}
	for _, want := range []string{"U-Boot 2022.04", "silent"} {
		if !strings.Contains(err.Error(), want) {
			t.Errorf("error %q must mention %q", err, want)
		}
	}
}

// TestConsolePromptTimeout: no shell within the budget is a console error too.
func TestConsolePromptTimeout(t *testing.T) {
	f := newFakeConsole(t)
	done := make(chan error, 1)
	go func() { done <- f.c.answerLogin(newLogin("root")) }()
	f.event(evPromptTimeout)
	if err := <-done; err == nil || !strings.Contains(err.Error(), "no shell prompt") {
		t.Fatalf("got %v, want a 'no shell prompt' error", err)
	}
}

// TestConsoleEndWithoutBegin: the END tag is not trusted before the shell's
// own BEGIN tag, because the echo of the whole frame contains both.
func TestConsoleEndWithoutBegin(t *testing.T) {
	f := newFakeConsole(t)
	if err := f.answerLoginAtPrompt(); err != nil {
		t.Fatal(err)
	}
	done := make(chan int, 1)
	go func() {
		code, _ := f.c.run("echo hi")
		done <- code
	}()
	// Only the echo so far; its END text must not end the run.
	f.say("echo __SRUN_B__n0nce; { echo hi\n}; echo __SRUN_E__n0nce:$?\r\r\n")
	select {
	case code := <-done:
		t.Fatalf("the run ended on the echoed frame with %d", code)
	case <-time.After(50 * time.Millisecond):
	}
	f.say("__SRUN_B__n0nce\r\r\nhi\r\r\n__SRUN_E__n0nce:0\r\r\n")
	select {
	case code := <-done:
		if code != 0 {
			t.Fatalf("status %d, want 0", code)
		}
	case <-time.After(time.Second):
		t.Fatal("the real tags did not end the run")
	}
}

// TestConsoleStreamsOutput: each line reaches stdout before the next arrives,
// so a long command shows progress.
func TestConsoleStreamsOutput(t *testing.T) {
	f := newFakeConsole(t)
	if err := f.answerLoginAtPrompt(); err != nil {
		t.Fatal(err)
	}
	done := make(chan int, 1)
	go func() {
		code, _ := f.c.run("seq 3")
		done <- code
	}()
	f.say("__SRUN_B__n0nce\r\r\n1\r\r\n")
	waitFor(t, func() bool { return strings.Contains(f.out.String(), "1\n") }, "the first line of output")
	f.say("2\r\r\n3\r\r\n__SRUN_E__n0nce:0\r\r\n")
	<-done
	if got := f.out.String(); got != "1\n2\n3\n" {
		t.Fatalf("stdout = %q", got)
	}
}

// TestConsoleQuiet: with -quiet the program leaves the console-noise writer
// nil, so the login dialogue produces no output at all while the command's
// own output still arrives on stdout.
func TestConsoleQuiet(t *testing.T) {
	f := newFakeConsole(t)
	f.c.errw = nil // the program's -quiet leaves the noise writer nil
	if err := f.answerLoginAtPrompt(); err != nil {
		t.Fatal(err)
	}
	if f.noise.Len() != 0 {
		t.Fatalf("quiet session wrote %q to stderr", f.noise.String())
	}
	done := make(chan int, 1)
	go func() {
		code, _ := f.c.run("echo hi")
		done <- code
	}()
	f.say("__SRUN_B__n0nce\r\r\nhi\r\r\n__SRUN_E__n0nce:0\r\r\n")
	if code := <-done; code != 0 {
		t.Fatalf("status %d, want 0", code)
	}
	if got := f.out.String(); got != "hi\n" {
		t.Fatalf("stdout = %q, want %q", got, "hi\n")
	}
	if f.noise.Len() != 0 {
		t.Fatalf("quiet session wrote %q to stderr", f.noise.String())
	}
}

// helpers ---------------------------------------------------------------

// answerLoginAtPrompt puts a console at its shell prompt, which is the state
// every run-phase test starts from. The prompt is queued before the session
// starts reading, so no goroutine is needed.
func (f *fakeConsole) answerLoginAtPrompt() error {
	f.port.events <- event{kind: evLine, line: "root@board:~# "}
	return f.c.answerLogin(newLogin("root"))
}

func itoa(n int) string {
	if n == 0 {
		return "0"
	}
	var b []byte
	for n > 0 {
		b = append([]byte{byte('0' + n%10)}, b...)
		n /= 10
	}
	return string(b)
}

func waitFor(t *testing.T, cond func() bool, what string) {
	t.Helper()
	deadline := time.Now().Add(2 * time.Second)
	for time.Now().Before(deadline) {
		if cond() {
			return
		}
		time.Sleep(time.Millisecond)
	}
	t.Fatalf("timed out waiting for %s", what)
}

// compile-time proof that the fake is what the session needs.
var (
	_ io.ReadWriter = (*fakePort)(nil)
)
