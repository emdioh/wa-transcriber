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
	errw   io.Writer // console noise, warnings; nil when -quiet

	idle   time.Duration
	prompt time.Duration
	dev    string

	markers  markers
	sawBegin bool // the shell's own BEGIN tag (not the echo) has arrived
	seen     []string
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

// noise forwards a line that is not part of the command's output: the login
// dialogue, the echoes of our own frame, anything outside the markers.
func (c *console) noise(line string) {
	if c.errw != nil && strings.TrimSpace(line) != "" {
		fmt.Fprintln(c.errw, line)
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
// stands alone on its own line. Nothing is sent before the board asks for it,
// so a console that is already at a shell never sees "root" typed into it.
func (c *console) answerLogin(l *login) error {
	var partial string
	for {
		select {
		case ev := <-c.events:
			switch ev.kind {
			case evLine:
				c.note(ev.line)
				if c.errw != nil && strings.TrimSpace(ev.line) != "" {
					fmt.Fprintln(c.errw, ev.line)
				}
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

// send writes a login answer to the board and mentions it on stderr, so the
// default (non-quiet) output shows the dance.
func (c *console) send(b []byte) error {
	if _, err := c.rw.Write(b); err != nil {
		return c.fail(stepPrompt, "writing %q: %v", strings.TrimSpace(string(b)), err)
	}
	if c.errw != nil {
		fmt.Fprintf(c.errw, "(sent %q)\n", strings.TrimSpace(string(b)))
	}
	return nil
}

// run sends the framed command and waits for the END tag. Every line the
// board prints inside the frame goes out as it arrives, so a long command
// shows progress; the moment the END line is parsed, the status is returned
// and nothing else is read.
func (c *console) run(cmd string) (int, error) {
	if _, err := io.WriteString(c.rw, c.markers.command(cmd)); err != nil {
		return 0, c.fail(stepRun, "tag lost, nothing to wait for: %v", err)
	}
	var partial string
	for {
		select {
		case ev := <-c.events:
			switch ev.kind {
			case evLine:
				status, done, err := c.handle(ev.line)
				if done || err != nil {
					return status, err
				}
			case evPartial:
				// A line the board has not finished. Before the BEGIN tag it
				// is the tty echoing our own frame; inside the frame it is
				// output that is still arriving (a progress bar, a prompt).
				if c.sawBegin && c.out != nil && strings.TrimSpace(ev.line) != "" {
					fmt.Fprintln(c.out, ev.line)
				}
				partial = ev.line
			case evClosed:
				if status, done, err := c.handle(partial); done || err != nil {
					return status, err
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

// handle processes one line during the run phase: it forwards the echo, cuts
// out everything inside the markers, and recognises the real END tag. A line
// that only looks like a tag is printed as output (tested).
func (c *console) handle(line string) (status int, done bool, err error) {
	if c.markers.isBegin(line) {
		if c.sawBegin {
			return 0, false, c.fail(stepRun, "the BEGIN tag appeared twice; the console is out of step")
		}
		c.sawBegin = true
		return 0, false, nil
	}
	if st, real, lookalike := c.markers.parseEnd(line); real {
		return st, true, nil
	} else if lookalike {
		// Someone printed something that resembles the tag without the
		// nonce, or mangled it. It is output, not the end.
		if c.errw != nil {
			fmt.Fprintf(c.errw, "serialrun: ignoring a tag-like line inside the output\n")
		}
	}
	if !c.sawBegin {
		// Everything before the shell's own BEGIN tag is the echo of the
		// command line (which contains the tags) or leftover console noise.
		c.noise(line)
		return 0, false, nil
	}
	// Output goes to stdout as it arrives, so a long command shows
	// progress instead of appearing all at once at the end.
	fmt.Fprintln(c.out, line)
	return 0, false, nil
}
