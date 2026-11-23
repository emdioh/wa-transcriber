// Command serialrun runs one command on a board over its USB serial console
// and returns the moment the command does, with its exit status.
//
//	go run ./tools/serialrun -dev /dev/ttyACM0 'ip -4 addr show eth0'
//	go run ./tools/serialrun 'uname -a'          # picks the newest ttyACM/ttyUSB
//
// It replaces tools/serial-run.sh, which kept the same one-open trick but
// decided when an answer was complete by reading until the line went quiet:
// every call cost seconds even when the command was instant, and a command
// that printed nothing for a while looked like it had finished. Here each
// step ends on its own event -- the login prompt, the password prompt, the
// shell prompt, the END tag, and only as a last resort a deadline that says
// the console has gone completely silent -- and the port is read with
// select(2) on a single fd held open from the first byte to the last.
//
// Exit status: the board command's own; 2 for a console problem (no shell
// prompt, no END tag, dead port); 3 for a usage error.
//
// What it cannot do, and says so when it happens: a command that calls
// `exit` (or `exec`, `reboot`, `poweroff`) takes the shell down with it, so no
// END tag follows and the call ends as a console error (2) -- run such a
// command as `sh -c '...'` to keep the frame's shell alive. A command that
// contains an unquoted '#' is run in a nested `sh -c`, so it does not share
// the frame shell's variables or working directory; that is the price of not
// letting the '#' comment out the END tag.
package main

import (
	"crypto/rand"
	"encoding/hex"
	"flag"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"sort"
	"strings"
	"time"
)

// Exit statuses that are ours, not the command's.
const (
	exitConsole = 2
	exitUsage   = 3
)

func main() { os.Exit(run(os.Args[1:], os.Stdout, os.Stderr)) }

// run is main without the os.Exit, so the tests drive the real program.
func run(args []string, stdout, stderr io.Writer) int {
	fs := flag.NewFlagSet("serialrun", flag.ContinueOnError)
	fs.SetOutput(stderr)
	var (
		dev     = fs.String("dev", "", "serial device (default: the most recent /dev/ttyACM* or /dev/ttyUSB*)")
		user    = fs.String("user", "root", "login name sent when the board asks for one; the password is empty")
		timeout = fs.Duration("timeout", 15*time.Second, "give up after this long, whatever the console is doing (the one bound on a hung or silent board; raise it for a long command)")
		idle    = fs.Duration("idle", 0, "give up after the console has been silent this long (default: the timeout; it is a safety net, never the mechanism that ends a run)")
		prompt  = fs.Duration("prompt", 0, "how long the board may keep the shell prompt from appearing (default: the timeout)")
		quiet   = fs.Bool("quiet", false, "same as the default, which already prints only the command's output: kept so old callers keep working")
		verbose = fs.Bool("verbose", false, "also show the console's side on stderr: the login dialogue, the answers sent, a note about tag-like lines in the output")
	)
	usage := func(msg string) int {
		if msg != "" {
			fmt.Fprintf(stderr, "serialrun: %s\n", msg)
		}
		fmt.Fprint(stderr, "usage: serialrun [-dev DEV] [-user NAME] [-timeout DUR] [-idle DUR] [-prompt DUR] [-verbose] 'command'\n")
		return exitUsage
	}
	if err := fs.Parse(args); err != nil {
		if err == flag.ErrHelp {
			return 0
		}
		return exitUsage
	}
	rest := fs.Args()
	switch {
	case len(rest) == 0:
		return usage("missing the command")
	case len(rest) > 1:
		return usage("give the whole command as one quoted argument")
	case strings.TrimSpace(rest[0]) == "":
		return usage("the command is empty")
	case *timeout <= 0:
		return usage("-timeout must be positive")
	}
	// The silence guard and the prompt wait default to the whole budget: one
	// flag bounds a hung board, the other two only refine it and say why.
	if *idle == 0 {
		*idle = *timeout
	}
	if *prompt == 0 {
		*prompt = *timeout
	}
	if *idle <= 0 || *prompt <= 0 {
		return usage("-idle and -prompt must be positive")
	}
	if *dev == "" {
		var err error
		if *dev, err = defaultDev(); err != nil {
			fmt.Fprintf(stderr, "serialrun: %v\n", err)
			return exitConsole
		}
	}

	opts := sessionOptions{dev: *dev, user: *user, timeout: *timeout, idle: *idle, prompt: *prompt, quiet: *quiet, verbose: *verbose, cmd: rest[0]}
	code, err := session(opts, stdout, stderr)
	if err != nil {
		fmt.Fprintf(stderr, "serialrun: %v\n", err)
		return exitConsole
	}
	return code
}

// session opens the port and runs the session on it. It exists so the tests
// can hand in an already-open descriptor (a pty pair) instead of a device
// path, which is the whole point of keeping converse separate.
func session(o sessionOptions, stdout, stderr io.Writer) (int, error) {
	// One open fd for the whole session. The board writes a CDC-ACM frame as
	// soon as it has something to say; a process that opens the read side
	// afterwards has that frame dropped by the kernel, which is what lost
	// whole answers. So: open read-write, keep the descriptor for the whole
	// session, and never close and reopen it between phases.
	f, err := os.OpenFile(o.dev, os.O_RDWR|syscallNoCTTY, 0)
	if err != nil {
		return 0, &consoleError{dev: o.dev, msg: "open: " + err.Error()}
	}
	defer f.Close()
	// Wake the console before the reader starts: getty prints its prompt only
	// when input arrives, and a prompt printed before this process opened the
	// port was dropped by the kernel. Sending it here, with nobody listening
	// yet, leaves its echo waiting in the device instead of racing the first
	// read; a bare CR is an empty line at a shell prompt and nothing at all at
	// "login:".
	if _, err := f.WriteString("\r"); err != nil {
		return 0, &consoleError{dev: o.dev, msg: "wake the console: " + err.Error()}
	}
	return runOn(f, o, stdout, stderr)
}

// runOn is the session on an open descriptor: raw mode on the host side (for a
// tty; a pty master answers ENOTTY and needs none), then the login dance and
// the framed command. The tests call it directly.
func runOn(f *os.File, o sessionOptions, stdout, stderr io.Writer) (int, error) {
	if restore, err := prepareHost(f); err == nil {
		defer restore()
	}
	// One hard deadline for the whole call, whatever phase it is in. The
	// idle and prompt deadlines below are refinements (they say *why* the
	// board went quiet); this one is the guarantee that a call always ends,
	// which is what makes the tool safe to use from a script.
	done := make(chan struct{})
	defer close(done)
	go func() {
		select {
		case <-done:
			return
		case <-time.After(o.timeout):
			fmt.Fprintf(stderr, "serialrun: gave up after %s (%s); the board said nothing to finish the call\n", o.timeout, o.dev)
			// The reader may be blocked on the port; exiting the process is
			// the only thing that reliably gives the console back.
			os.Exit(2)
		}
	}()
	p := newPort(f, o.idle, o.prompt)
	p.start()
	// The reader is left to notice the stop on its own: waiting for it here
	// would add however long its current poll lasts to every call, which is
	// exactly the fixed delay this tool exists to remove.
	defer p.stop()
	return converse(p, o, stdout, stderr)
}

// sessionOptions is what the program was asked to do.
type sessionOptions struct {
	dev  string
	user string
	// timeout bounds the whole call: prompt, login and command together. It
	// is the one guarantee against a hung or silent console.
	timeout time.Duration
	idle    time.Duration
	prompt  time.Duration
	quiet   bool
	// verbose shows the console's side of the session on stderr. It is off
	// by default and independent of quiet: stdout is the command's output
	// either way, stderr is the console's own talk and problems.
	verbose bool
	cmd     string
}

// converse is the session itself: it needs the port's event stream and bytes
// to write, so the tests can drive it with a fake io.ReadWriter instead.
func converse(p *port, o sessionOptions, stdout, stderr io.Writer) (int, error) {
	nonce, err := newNonce()
	if err != nil {
		return 0, &consoleError{dev: o.dev, msg: "random nonce: " + err.Error()}
	}
	c := &console{
		rw:      p.rw,
		events:  p.events,
		out:     stdout,
		errw:    stderr,
		verbose: o.verbose && !o.quiet,
		idle:    o.idle,
		prompt:  o.prompt,
		dev:     o.dev,
		markers: markers{
			begin: "__SRUN_B__" + nonce,
			end:   "__SRUN_E__" + nonce,
		},
	}
	if err := c.answerLogin(newLogin(o.user)); err != nil {
		return 0, err
	}
	// From here on only the silence safety net applies: a command may run as
	// long as it likes, and the events that belonged to the login dialogue
	// must not be read as its answer.
	p.phaseStart(time.Now())
	return c.run(o.cmd)
}

// defaultDev picks the port like the shell script did: the most recently
// created device node, because the BBB's gadget re-enumerates on every reboot
// and ttyACM0 and ttyACM1 swap places.
func defaultDev() (string, error) {
	var found []string
	for _, pattern := range []string{"/dev/ttyACM*", "/dev/ttyUSB*"} {
		matches, err := filepath.Glob(pattern)
		if err == nil {
			found = append(found, matches...)
		}
	}
	if len(found) == 0 {
		return "", &consoleError{msg: "no /dev/ttyACM* or /dev/ttyUSB* device; use -dev"}
	}
	sort.SliceStable(found, func(i, j int) bool {
		si, err1 := os.Stat(found[i])
		sj, err2 := os.Stat(found[j])
		if err1 != nil || err2 != nil {
			return found[i] < found[j]
		}
		return si.ModTime().After(sj.ModTime())
	})
	return found[0], nil
}

// newNonce makes the random tag suffix.
func newNonce() (string, error) {
	b := make([]byte, nonceBytes)
	if _, err := rand.Read(b); err != nil {
		return "", err
	}
	return hex.EncodeToString(b), nil
}
