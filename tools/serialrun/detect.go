package main

import "strings"

// The console is shared with getty, motd, kernel messages and whatever a long
// command prints, so every detection here looks only at how a line *ends*: the
// shell prompt, the login prompt and the password prompt are all the last
// thing on the board's line. A line that merely contains such text ('grep
// login: /etc/passwd' echoing, "ash: root: not found") must not match.

// endsInShellPrompt reports whether line ends with a shell prompt: the
// conventional "# " for root, "$ " for a user, or the same without the space
// when the tty truncated it. An empty trailing space run is allowed because
// the line has been trimmed of its terminators, not of its spaces.
func endsInShellPrompt(line string) bool {
	t := strings.TrimRight(line, " \t")
	if t == "" {
		return false
	}
	switch t[len(t)-1] {
	case '#', '$':
		return true
	}
	return false
}

// endsInLogin reports a getty login prompt ("hostname login:", "Debian
// GNU/Linux ... login:"). getty writes it with no newline after it, so the
// line here is whatever stood before the prompt once the stream went quiet.
func endsInLogin(line string) bool {
	return strings.HasSuffix(strings.ToLower(line), "login:")
}

// endsInPassword reports the getty prompt for the account's secret. On some
// gettys it is preceded by the user name on the same line, so only the tail
// counts.
func endsInPassword(line string) bool {
	t := strings.TrimRight(line, " ")
	if !strings.HasSuffix(t, ":") {
		return false
	}
	t = strings.TrimSuffix(t, ":")
	return strings.HasSuffix(strings.ToLower(t), "password")
}
