package main

import "testing"

func TestEndsInShellPrompt(t *testing.T) {
	tests := []struct {
		line string
		want bool
	}{
		{"# ", true},
		{"#", true},
		{"$ ", true},
		{"$", true},
		{"root@beaglebone:~# ", true},
		{"-sh: 1: root: not found", false},
		{"login:", false},
		{"", false},
		{"   ", false},
		{"$ ./script.sh", false},
		{"# a comment", false},
	}
	for _, tt := range tests {
		if got := endsInShellPrompt(tt.line); got != tt.want {
			t.Errorf("endsInShellPrompt(%q) = %v, want %v", tt.line, got, tt.want)
		}
	}
}

func TestEndsInLogin(t *testing.T) {
	tests := []struct {
		line string
		want bool
	}{
		{"beaglebone login:", true},
		{"Debian GNU/Linux 12 beaglebone ttyGS0", false},
		{"beaglebone Login:", true},
		{"login:", true},
		{"login: root", false},
		{"grep login: /etc/passwd", false},
		{"", false},
	}
	for _, tt := range tests {
		if got := endsInLogin(tt.line); got != tt.want {
			t.Errorf("endsInLogin(%q) = %v, want %v", tt.line, got, tt.want)
		}
	}
}

func TestEndsInPassword(t *testing.T) {
	tests := []struct {
		line string
		want bool
	}{
		{"Password:", true},
		{"Password: ", true},
		{"beaglebone Password:", true},
		{"password:", true},
		{"Passwd:", false},
		{"Password: wrong", false},
		{"", false},
	}
	for _, tt := range tests {
		if got := endsInPassword(tt.line); got != tt.want {
			t.Errorf("endsInPassword(%q) = %v, want %v", tt.line, got, tt.want)
		}
	}
}

// TestLoginSequence is the decision sequence the task asks for, as a pure
// state machine: a login prompt asks for the user name, the password prompt
// for an empty line, and only the shell prompt allows the command.
func TestLoginSequence(t *testing.T) {
	l := newLogin("root")

	if l.state != promptState {
		t.Fatalf("a session starts waiting for a prompt, got %v", l.state)
	}

	// A live shell's output must not trigger anything, and neither must
	// leftovers from an earlier run.
	for _, line := range []string{
		"-sh: root: not found",
		"echo __SRUN_B__deadbeef; { uptime",
		" 10:31:04 up 3 days,  1 user,  load average: 0.00",
		"",
	} {
		if send, ready := l.answer(line); send != nil || ready {
			t.Fatalf("line %q must not be answered, got send=%q ready=%v", line, send, ready)
		}
	}

	// The board asks: send the user name, once.
	send, ready := l.answer("beaglebone login:")
	if string(send) != "root\r" {
		t.Fatalf("login prompt: got %q, want %q", send, "root\r")
	}
	if ready {
		t.Fatal("a login prompt is not a shell")
	}
	if l.state != userState {
		t.Fatalf("state after the login prompt = %v, want %v", l.state, userState)
	}

	// The user name comes back echoed.
	if send, ready := l.answer("root"); send != nil || ready {
		t.Fatalf("the echo must not be answered, got send=%q ready=%v", send, ready)
	}

	// Empty password: a bare \r.
	send, ready = l.answer("Password:")
	if string(send) != "\r" {
		t.Fatalf("password prompt: got %q, want a bare CR", send)
	}
	if ready {
		t.Fatal("a password prompt is not a shell")
	}
	if l.state != passwordState {
		t.Fatalf("state after the password prompt = %v, want %v", l.state, passwordState)
	}

	// Boot noise between the password and the prompt: still nothing sent.
	for _, line := range []string{
		"Debian GNU/Linux 12",
		"root@beaglebone:~# echo leftovers",
	} {
		if send, _ := l.answer(line); send != nil {
			t.Fatalf("%q must not be answered, got %q", line, send)
		}
	}

	send, ready = l.answer("root@beaglebone:~# ")
	if send != nil || !ready {
		t.Fatalf("shell prompt: got send=%q ready=%v, want nothing sent and ready", send, ready)
	}
	if l.state != shellState {
		t.Fatalf("state after the shell prompt = %v, want %v", l.state, shellState)
	}
}

// TestLoginNeverTypesIntoAShell is the dangerous transition: with a shell
// already at a prompt, the user name must never be sent as a command.
func TestLoginNeverTypesIntoAShell(t *testing.T) {
	l := newLogin("root")
	for _, line := range []string{"# ", "-sh: root: not found\r", "# "} {
		send, _ := l.answer(line)
		if len(send) > 0 && string(send) != "" {
			if string(send) == "root\r" {
				t.Fatalf("the user name was typed into a live shell after %q", line)
			}
		}
		if line == "# " && l.state != shellState {
			t.Fatalf("state = %v, want shell", l.state)
		}
	}
}

// TestLoginRetriesTheName covers a getty that reprinted its prompt because
// nothing arrived: the user name is sent again rather than giving up.
func TestLoginRetriesTheName(t *testing.T) {
	l := newLogin("root")
	l.answer("beaglebone login:")
	send, _ := l.answer("beaglebone login:")
	if string(send) != "root\r" {
		t.Fatalf("a repeated login prompt must send the name again, got %q", send)
	}
}
