//go:build linux

package main

import (
	"io"
	"syscall"
	"time"
	"unsafe"
)

// syscallNoCTTY keeps the open from becoming the process's controlling
// terminal: a tool that grabbed the console as its controlling tty would get
// SIGINT from the line discipline whenever the board's getty sent a break,
// and SIGTTOU/SIGTTIN on its own writes when run in the background.
const syscallNoCTTY = syscall.O_NOCTTY

// pollFd waits for a descriptor to become readable. It is poll(2) and not
// select(2) because a pty in the tests can sit above FD_SETSIZE on a busy
// machine, where select(2) would simply ignore it. The port is a character
// device with a single kind of readiness, so POLLIN is the whole story.
func pollFd(fd uintptr, timeout time.Duration) (bool, error) {
	type pollFd struct {
		Fd      int32
		Events  int16
		Revents int16
	}
	const pollIn = 0x1
	pfd := pollFd{Fd: int32(fd), Events: pollIn}
	ms := -1
	if timeout >= 0 {
		ms = int(timeout / time.Millisecond)
	}
	n, _, errno := syscall.Syscall(syscall.SYS_POLL, uintptr(unsafe.Pointer(&pfd)), 1, uintptr(ms))
	if errno != 0 {
		if errno == syscall.EINTR {
			return false, nil // an interrupted wait is just a timeout
		}
		return false, errno
	}
	return n > 0, nil
}

// waitReadableAny blocks until rw is readable or the timeout passes. A port
// backed by a file descriptor can be waited on; a plain io.ReadWriter (the
// tests' pipe pair) cannot, and is reported readable at once so its Read can
// return what it has.
func waitReadableAny(rw io.ReadWriter, timeout time.Duration) (bool, error) {
	f, ok := rw.(interface{ Fd() uintptr })
	if !ok {
		time.Sleep(minDuration(timeout, 5*time.Millisecond))
		return true, nil
	}
	return pollFd(f.Fd(), timeout)
}
