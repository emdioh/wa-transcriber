package main

import "strings"

// markers frames a command so its answer can be cut out of the console stream.
//
// The board's tty echoes the line we send, so the whole frame appears twice:
// once as the echo (with the command after the BEGIN tag) and once as the
// shell's own output, each tag on a line of its own. Only a line that *is* a
// tag counts, which is the rule the shell script's awk applied too.
//
// A second hazard is the answer itself: a command that prints the marker --
// `echo __SRUN_E__...`, or a file that happens to contain it -- must not end
// the run early. Hence the nonce: it is random per run, so no output can
// contain the real tag unless it came from the shell that ran the frame, and
// a fake tag is reported as ordinary output (see console.handle).
type markers struct {
	begin string // e.g. "__SRUN_B__9f3c1a..."
	end   string // e.g. "__SRUN_E__9f3c1a..."
}

// beginLine is what the shell prints before the command runs. It carries the
// nonce too, so a line that only looks like the begin tag in a previous
// session cannot start this one.
func (m markers) beginLine() string { return m.begin }

// command wraps cmd so that it runs in this shell (not a subshell: `cd`,
// variables and functions behave as on a terminal) and its status comes back
// in the END line. `{ ...; }` keeps a multi-word command one group and
// tolerates a trailing `;` in the user's text.
//
// A command containing an unquoted '#' gets special treatment: the shell
// would take the rest of the line, closing brace and END tag included, as a
// comment, and the run would hang until the idle safety net fired. The whole
// command is therefore handed to a nested `sh -c` with the text single-quoted,
// where the '#' is just data. The price is that such a command does not share
// this shell's variables or working directory; that is stated in the help
// text rather than guessed at, because the alternative is a silent hang.
func (m markers) command(cmd string) string {
	body := cmd
	if strings.ContainsRune(cmd, '#') {
		body = "sh -c " + shellQuote(cmd)
	}
	return "echo " + m.begin + "; { " + body + "; }; echo " + m.end + ":$?\r"
}

// shellQuote wraps s in single quotes for a POSIX shell, escaping the quotes
// it contains.
func shellQuote(s string) string {
	return "'" + strings.ReplaceAll(s, "'", `'\''`) + "'"
}

// parseEnd reports the exit status when line is the real END tag, and
// whether it is a line that only resembles one (a fake tag that must be
// printed as output, or a mangled tag that must not be).
func (m markers) parseEnd(line string) (status int, real, lookalike bool) {
	if line == m.end {
		// The shell always writes ":$?", so a bare tag cannot be ours.
		return 0, false, true
	}
	prefix := m.end + ":"
	if len(line) < len(prefix) || line[:len(prefix)] != prefix {
		return 0, false, false
	}
	digits := line[len(prefix):]
	if digits == "" {
		return 0, false, true
	}
	n := 0
	for _, c := range digits {
		if c < '0' || c > '9' {
			return 0, false, true
		}
		n = n*10 + int(c-'0')
		if n > 255 {
			return 0, false, true
		}
	}
	return n, true, false
}

// isBegin reports whether line is the real BEGIN tag standing alone.
func (m markers) isBegin(line string) bool { return line == m.begin }

// nonceBytes is how much randomness goes into a tag. Eight bytes of a random
// source are plenty: the chance that the board prints the same tag by accident
// is nil, and a command that prints a *guessed* tag would need to read this
// session's own command line, which is exactly the echo we throw away.
const nonceBytes = 8
