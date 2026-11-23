package main

// login is the console state machine. It is pure: it looks at one line at a
// time and says what, if anything, to send back. Everything the shell script
// did with sleeping is expressed here as a reaction to text the board printed
// -- the login prompt, the password prompt, the shell prompt -- so a call
// costs whatever the board takes and nothing more.
//
// The dangerous transition is prompting -> shell: sending "root" to a live
// shell runs it as a command ("-sh: root: not found"). It therefore only
// happens when the board itself asked for it (a login prompt has been seen),
// which is also why the state machine starts in promptState and waits.
type loginState int

const (
	// promptState: waiting for a shell prompt; nothing is sent.
	promptState loginState = iota
	// userState: a login prompt was seen, the user name is sent once.
	userState
	// passwordState: a password prompt was seen, an empty line is sent.
	passwordState
	// shellState: the shell prompt arrived; the command may be sent.
	shellState
)

func (s loginState) String() string {
	switch s {
	case promptState:
		return "waiting for a shell prompt"
	case userState:
		return "login prompt seen"
	case passwordState:
		return "password prompt seen"
	case shellState:
		return "shell prompt seen"
	}
	return "unknown"
}

// login decides the session. Its zero value-ready constructor is newLogin;
// the zero value is promptState, which is what a session starts with.
type login struct {
	state loginState
	user  string
	// seenLogin and seenPassword remember that the board asked, so a shell
	// prompt is only trusted after a login prompt for this session.
	seenLogin bool
}

func newLogin(user string) *login { return &login{user: user} }

// answer takes the next complete line from the console and returns what to
// write to it (nil for nothing) and whether that finished the login: once the
// shell prompt has been seen, the caller may send the command. A line is
// complete when it stood alone in the stream, which the caller guarantees.
//
// Only the *end* of the line is inspected: "debian login:" and "# " match,
// while "ash: root: not found" (the echo of a command) does not, so a shell's
// own output cannot be mistaken for a prompt.
func (l *login) answer(line string) (send []byte, ready bool) {
	if endsInShellPrompt(line) {
		l.state = shellState
		return nil, true
	}
	switch l.state {
	case promptState:
		if endsInLogin(line) {
			l.seenLogin = true
			l.state = userState
			return []byte(l.user + "\r"), false
		}
	case userState:
		// The user name is echoed back; the next line that ends in a
		// password prompt is the request for its value.
		if endsInPassword(line) {
			l.state = passwordState
			return []byte("\r"), false
		}
		if endsInLogin(line) {
			// The previous user name fell into the gap; try once more.
			return []byte(l.user + "\r"), false
		}
	case passwordState:
		// An empty password logs in at once; no further prompt is
		// expected, so just wait for the shell prompt. A second password
		// prompt means the first empty line was swallowed: send again.
		if endsInPassword(line) {
			return []byte("\r"), false
		}
	}
	return nil, false
}
