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
		dev    = fs.String("dev", "", "serial device (default: the most recent /dev/ttyACM* or /dev/ttyUSB*)")
		user   = fs.String("user", "root", "login name sent when the board asks for one; the password is empty")
		idle   = fs.Duration("idle", 120*time.Second, "safety net for a hung board: give up after the console has been silent this long (never the mechanism that ends a run)")
		prompt = fs.Duration("prompt", 20*time.Second, "how long the board may keep the shell prompt from appearing")
		quiet  = fs.Bool("quiet", false, "print only the command's output on stdout; no console noise to stderr")
	)
	usage := func(msg string) int {
		if msg != "" {
			fmt.Fprintf(stderr, "serialrun: %s\n", msg)
		}
		fmt.Fprint(stderr, "usage: serialrun [-dev DEV] [-user NAME] [-idle DUR] [-prompt DUR] [-quiet] 'command'\n")
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
	case *idle <= 0:
		return usage("-idle must be positive (it is a safety net, not a wait)")
	case *prompt <= 0:
		return usage("-prompt must be positive")
	}
	if *dev == "" {
		var err error
		if *dev, err = defaultDev(); err != nil {
			fmt.Fprintf(stderr, "serialrun: %v\n", err)
			return exitConsole
		}
	}

	opts := sessionOptions{dev: *dev, user: *user, idle: *idle, prompt: *prompt, quiet: *quiet, cmd: rest[0]}
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
	return runOn(f, o, stdout, stderr)
}

// runOn is the session on an open descriptor: raw mode on the host side (for a
// tty; a pty master answers ENOTTY and needs none), then the login dance and
// the framed command. The tests call it directly.
func runOn(f *os.File, o sessionOptions, stdout, stderr io.Writer) (int, error) {
	if restore, err := prepareHost(f); err == nil {
		defer restore()
	}
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
	dev    string
	user   string
	idle   time.Duration
	prompt time.Duration
	quiet  bool
	cmd    string
}

// converse is the session itself: it needs the port's event stream and bytes
// to write, so the tests can drive it with a fake io.ReadWriter instead.
func converse(p *port, o sessionOptions, stdout, stderr io.Writer) (int, error) {
	nonce, err := newNonce()
	if err != nil {
		return 0, &consoleError{dev: o.dev, msg: "random nonce: " + err.Error()}
	}
	c := &console{
		rw:     p.rw,
		events: p.events,
		errw:   stderr,
		idle:   o.idle,
		prompt: o.prompt,
		dev:    o.dev,
		markers: markers{
			begin: "__SRUN_B__" + nonce,
			end:   "__SRUN_E__" + nonce,
		},
	}
	if !o.quiet {
		c.out = stdout
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
