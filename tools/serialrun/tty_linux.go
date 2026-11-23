//go:build linux

package main

import (
	"os"
	"syscall"
	"unsafe"
)

// prepareHost puts the host-side tty into raw mode and returns the restore
// function. Without it the host line discipline would rewrite the board's
// bytes before we see them: ICRNL turns a lone \r into \n, and echo/canonical
// mode would add a second response to the board's own echo.
//
// A pty master is not a tty with a line discipline of its own -- its slave
// is -- and setting the master's termios stops the slave's bytes from being
// delivered, so a master is left alone (the pty test harness relies on this,
// and it is also why a dev run under a tty multiplexer still works).
func prepareHost(f *os.File) (func(), error) {
	if isPtyMaster(f) {
		return func() {}, nil
	}
	return makeRaw(f)
}

// isPtyMaster reports whether f is the master side of a pseudo-terminal:
// TIOCGPTN answers with the slave number on a master and fails on a real tty
// (the number it returns is meaningless, but the ioctl succeeding is the
// signal).
func isPtyMaster(f *os.File) bool {
	var n uint32
	return ioctl(f, syscall.TIOCGPTN, unsafe.Pointer(&n)) == nil
}
