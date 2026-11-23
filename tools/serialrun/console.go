package main

import (
	"fmt"
	"io"
	"strings"
	"time"
)

// event is what the reader hands to the session: a completed line, a read
// error, the end of the stream, or one of the two deadlines. Everything the
// session does happens in reaction to one of these; no phase ever sleeps.
type eventKind int

const (
	evLine    eventKind = iota
	evPartial           // a chunk ended; line holds the unterminated remainder
	evReadErr
	evClosed
	evPromptTimeout
	evIdleTimeout
)

type event struct {
	kind eventKind
	line string
	err  error
}

// step is how far the session got; it names the phase in error messages.
type step int

const (
	stepPrompt step = iota // waiting for the shell prompt
	stepRun                // the command frame was sent, waiting for the END tag
)

func (s step) String() string {
	if s == stepRun {
		return "running the command"
	}
	return "waiting for a shell prompt"
}

// console is one session on the port.
type console struct {
	rw     io.ReadWriter
	events <-chan event
	out    io.Writer // the command's output, streamed as it arrives
	errw   io.Writer // problems; console noise too when verbose
	// verbose shows the console's side of the session (the login dialogue,
	// the answers sent) on errw. It is off by default: stderr carries only
	// what went wrong, and stdout carries the command's output.
	verbose bool

	idle   time.Duration
	prompt time.Duration
	dev    string

	markers markers
	// sawBegin: the shell's own BEGIN tag, not the echo of the frame, has
	// arrived. Until then the console is still echoing what we typed.
	sawBegin bool
	// echo and echoes handle that echo: echo holds what the tty has read
	// back so far, echoes is the frame as it was written (see echoing).
	echo   strings.Builder
	echoes string
	seen   []string
}

const seenMax = 10

func (c *console) fail(s step, format string, args ...any) error {
	return &consoleError{step: s, dev: c.dev, msg: fmt.Sprintf(format, args...)}
}

func (c *console) note(line string) {
	if strings.TrimSpace(line) == "" {
		return
	}
	c.seen = append(c.seen, line)
	if len(c.seen) > seenMax {
		c.seen = c.seen[len(c.seen)-seenMax:]
	}
}

// lastLines renders what the console showed, for a failure message.
func (c *console) lastLines() string {
	if len(c.seen) == 0 {
		return "    (nothing)"
	}
	lines := make([]string, len(c.seen))
	for i, l := range c.seen {
		lines[i] = "    " + l
	}
	return strings.Join(lines, "\n")
}

// noise reports a line that is not part of the command's output: the login
// dialogue, leftover console chatter. It goes to stderr and only when -verbose
// asked for the console's side; it never goes to stdout, so no line can ever
// appear on both streams.
func (c *console) noise(line string) {
	if c.verbose && c.errw != nil && strings.TrimSpace(line) != "" {
		fmt.Fprintln(c.errw, line)
	}
}

// emit writes one line of the command's output. Everything printed between
// the markers comes through here and nowhere else, and it goes out the moment
// it arrives, so a long command shows progress.
func (c *console) emit(line string) {
	if c.out != nil {
		fmt.Fprintln(c.out, line)
	}
}

// consoleError is a failure of the session rather than of the command: no
// shell, no END tag, a dead port. Its exit status is 2.
type consoleError struct {
	step step
	dev  string
	msg  string
}

func (e *consoleError) Error() string {
	if e.dev == "" {
		return e.msg
	}
	return e.dev + ": " + e.msg
}

// errUsage is a bad invocation; its exit status is 3.
type errUsage struct{ msg string }

func (e *errUsage) Error() string { return e.msg }

// answerLogin drives the login state machine until the board's shell prompt
// stands alone on its own line. The wake-up carriage return is sent by the
// caller before the first read (see session), so a console that is already at
// a shell never sees "root" typed into it.
func (c *console) answerLogin(l *login) error {
	var partial string
	for {
		select {
		case ev := <-c.events:
			switch ev.kind {
			case evLine:
				c.note(ev.line)
				c.noise(ev.line)
				send, ready := l.answer(ev.line)
				if ready {
					return nil
				}
				if send != nil {
					if err := c.send(send); err != nil {
						return err
					}
				}
			case evPartial:

				// The login prompt is printed without a newline and the
				// board then stops talking, so it only ever arrives here:
				// the text is complete in the sense that matters (the board
				// has nothing more to say), and answering it on the *next*
				// chunk would wait for a line that never ends.
				partial = ev.line
				send, ready := l.answer(partial)
				if ready {
					return nil
				}
				if send != nil {
					if err := c.send(send); err != nil {
						return err
					}
				}
			case evClosed:
				// A prompt with no newline after it only shows up here: the
				// board printed it and stopped talking. It was already noted
				// when it arrived, so it is not noted again.
				return c.fail(stepPrompt, "the console closed (%v); last lines:\n%s",
					l.state, c.lastLines())
			case evPromptTimeout:
				return c.fail(stepPrompt, "no shell prompt within %s (%v); last lines:\n%s",
					c.prompt, l.state, c.lastLines())
			case evIdleTimeout:
				return c.fail(stepPrompt, "console silent for %s while %v; last lines:\n%s",
					c.idle, l.state, c.lastLines())
			case evReadErr:
				return c.fail(stepPrompt, "read: %v", ev.err)
			}
		}
	}
}

// send writes a login answer to the board and, with -verbose, mentions it on
// stderr so the dance can be followed.
func (c *console) send(b []byte) error {
	if _, err := c.rw.Write(b); err != nil {
		return c.fail(stepPrompt, "writing %q: %v", strings.TrimSpace(string(b)), err)
	}
	if c.verbose && c.errw != nil {
		fmt.Fprintf(c.errw, "(sent %q)\n", strings.TrimSpace(string(b)))
	}
	return nil
}

// run sends the framed command and waits for the END tag. Every line the
// board prints inside the frame goes out as it arrives, so a long command
// shows progress; the moment the END line is parsed, the status is returned
// and nothing else is read.
func (c *console) run(cmd string) (int, error) {
	frame := c.markers.command(cmd)
	// The tty echoes the frame back before the shell prints anything, so
	// what it echoes has to be recognised and dropped (see echoing).
	c.echoes = frame
	if _, err := io.WriteString(c.rw, frame); err != nil {
		return 0, c.fail(stepRun, "tag lost, nothing to wait for: %v", err)
	}
	var partial string
	for {
		select {
		case ev := <-c.events:
			switch ev.kind {
			case evLine:
				if c.echoing(ev.line) {
					// That was still our own frame coming back.
					partial = ""
					continue
				}
				status, done, err := c.handle(ev.line)
				if done || err != nil {
					return status, err
				}
			case evPartial:
				// A line the board has not finished, which the port reports
				// on every chunk. While the frame is being echoed it is the
				// echo and is dropped; inside the frame it is remembered as
				// the line in progress but not printed, because the port
				// also delivers the finished line as evLine a moment later
				// and printing both is exactly how every output line used to
				// appear twice.
				if c.echoing(ev.line) {
					partial = ev.line
					continue
				}
				partial = ev.line
			case evClosed:
				// The console died. The line it died on is judged first: a
				// command that takes the tty down (`reboot -f` closes the
				// port on the host side) still ends with its END tag.
				if !c.echoing(partial) && partial != "" {
					if status, done, err := c.handle(partial); done || err != nil {
						return status, err
					}
				}
				return 0, c.fail(stepRun, "console closed before the END tag "+
					"(did the command call exit?); last lines:\n%s", c.lastLines())
			case evPromptTimeout:
				// Disabled once the frame is sent, so this is a race with
				// it; a command that is running must not be cut off.
				continue
			case evIdleTimeout:
				return 0, c.fail(stepRun, "console silent for %s while running the command "+
					"(still running, shell gone, or port lost); last lines:\n%s", c.idle, c.lastLines())
			case evReadErr:
				return 0, c.fail(stepRun, "read: %v", ev.err)
			}
		}
	}
}

// echoing records one piece of what the console sent after the frame was
// written, and reports whether it is nothing but the frame coming back.
//
// The tty echoes what we type as it arrives, in pieces of a few bytes that
// have no line break of their own. Those pieces look exactly like an
// unfinished line of output, and treating them as one is what put the
// command's own text -- both markers included -- on stdout and stderr as the
// "echo" and "END tag" lines. The test is "is what I have read so far a
// prefix of what I wrote", compared with line breaks ignored, rather than
// "does this piece mention a marker": the frame is the only thing on the wire
// between sending it and the shell's first line, and a command's own output
// cannot match the frame as a whole without literally reprinting it, markers
// and all.
//
// It returns true while the piece still belongs to the echo, false the moment
// it does not. From then on the echo is over and a line is judged on its own
// by handle.
func (c *console) echoing(piece string) bool {
	if c.sawBegin || c.echoes == "" {
		// The shell has printed its own BEGIN tag (the echo is over), or
		// there is nothing left to compare against.
		return false
	}
	c.echo.WriteString(piece)
	got := strings.Join(strings.Fields(c.echo.String()), " ")
	want := strings.Join(strings.Fields(c.echoes), " ")
	switch {
	case got == want:
		// The whole frame has come back.
		c.echo.Reset()
		c.echoes = ""
		return true
	case strings.HasPrefix(want, got):
		// Still inside the echo.
		return true
	}
	// Not the frame any more: the echo is over and this line is the shell's.
	c.echo.Reset()
	c.echoes = ""
	return false
}

// handle processes one line during the run phase: it cuts out everything
// inside the markers and recognises the real END tag. A line that only looks
// like a tag is printed as output (tested).
func (c *console) handle(line string) (status int, done bool, err error) {
	if c.markers.isBegin(line) {
		if c.sawBegin {
			return 0, false, c.fail(stepRun, "the BEGIN tag appeared twice; the console is out of step")
		}
		c.sawBegin = true
		return 0, false, nil
	}
	if st, real, lookalike := c.markers.parseEnd(line); real && c.sawBegin {
		return st, true, nil
	} else if lookalike && c.sawBegin && c.verbose {
		// Someone printed something that resembles the tag without the
		// nonce, or mangled it. It is output, not the end; the note is for
		// whoever is watching the console side.
		fmt.Fprintln(c.errw, "serialrun: ignoring a tag-like line inside the output")
	}
	if !c.sawBegin {
		// Nothing but the echo of the frame can precede the shell's own
		// BEGIN tag; what echoing did not claim is console chatter.
		c.noise(line)
		return 0, false, nil
	}
	c.emit(line)
	return 0, false, nil
}
