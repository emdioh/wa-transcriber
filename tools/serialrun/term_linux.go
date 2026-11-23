//go:build linux

package main

import (
	"fmt"
	"os"
	"syscall"
	"time"
	"unsafe"
)

// ioctl calls ioctl(2) on f. The argument is always a pointer to a word-sized
// value here (termios struct, int32, uint32), which is what the tty driver
// expects.
func ioctl(f *os.File, req uintptr, arg unsafe.Pointer) error {
	_, _, errno := syscall.Syscall(syscall.SYS_IOCTL, f.Fd(), req, uintptr(arg))
	if errno != 0 {
		return errno
	}
	return nil
}

// makeRaw puts the tty behind f into raw mode and returns a function that
// restores the previous settings.
//
// VMIN=0/VTIME=0 matters for the *real* port: a blocking read with VMIN=1
// would hang the reader on a board that went quiet, and the loop needs every
// read to return what is there and nothing more. It is deliberately not
// applied to a pty master (see prepareHost): a master is not a line
// discipline of its own, and forcing these modes on it stops the slave's
// output from being delivered at all -- the pty tests would then hang on a
// healthy shell.
//
// Raw mode removes everything the *host* would otherwise add to the bytes the
// board sent: ICRNL would turn a lone \r into \n, ONLCR would rewrite \n as
// \r\n on the way out (so our \r would arrive as \r\n), ECHO and ICANON would
// make the line discipline answer the board's own echo again, and ISIG would
// turn a Ctrl-C byte into a signal to *us*. VMIN=0/VTIME=0 makes reads
// return what is there and nothing more, which is what the select loop
// wants: it must never block waiting for a character.
func makeRaw(f *os.File) (func(), error) {
	var old syscall.Termios
	if err := ioctl(f, syscall.TCGETS, unsafe.Pointer(&old)); err != nil {
		return nil, fmt.Errorf("tcgetattr: %w", err)
	}
	raw := old
	applyRaw(&raw)
	// The tool never blocks in Read: a board that goes quiet must look like
	// "nothing to read", not like a stuck process.
	raw.Cc[syscall.VMIN] = 0
	raw.Cc[syscall.VTIME] = 0
	if err := ioctl(f, syscall.TCSETS, unsafe.Pointer(&raw)); err != nil {
		return nil, fmt.Errorf("tcsetattr: %w", err)
	}
	return func() { _ = ioctl(f, syscall.TCSETS, unsafe.Pointer(&old)) }, nil
}

// setRaw applies the raw flags in place, leaving the read timeout alone (the
// caller decides VMIN/VTIME: the tool wants zero, a shell that reads the same
// line wants one).
func setRaw(f *os.File) error {
	var raw syscall.Termios
	if err := ioctl(f, syscall.TCGETS, unsafe.Pointer(&raw)); err != nil {
		return fmt.Errorf("tcgetattr: %w", err)
	}
	applyRaw(&raw)
	raw.Cc[syscall.VMIN] = 1
	raw.Cc[syscall.VTIME] = 0
	if err := ioctl(f, syscall.TCSETS, unsafe.Pointer(&raw)); err != nil {
		return fmt.Errorf("tcsetattr: %w", err)
	}
	return nil
}

// applyRaw turns off the host's translations and echo.
func applyRaw(raw *syscall.Termios) {
	raw.Iflag &^= syscall.IGNBRK | syscall.BRKINT | syscall.PARMRK | syscall.ISTRIP |
		syscall.INLCR | syscall.IGNCR | syscall.ICRNL | syscall.IXON
	raw.Oflag &^= syscall.OPOST
	raw.Lflag &^= syscall.ECHO | syscall.ECHONL | syscall.ICANON | syscall.ISIG | syscall.IEXTEN
	raw.Cflag &^= syscall.CSIZE | syscall.PARENB
	raw.Cflag |= syscall.CS8
}

// setBaud sets the line speed when the user asked for something other than
// the default, which is what the board's getty already uses. The port is
// opened with whatever speed the driver kept, and the gadget ignores it, so
// the default path needs no lookup at all.
func setBaud(f *os.File, baud int) error {
	speed, ok := baudSpeeds[baud]
	if !ok {
		return fmt.Errorf("unsupported baud %d", baud)
	}
	var t syscall.Termios
	if err := ioctl(f, syscall.TCGETS, unsafe.Pointer(&t)); err != nil {
		return fmt.Errorf("tcgetattr: %w", err)
	}
	// CBAUD is 0x100f on every linux arch Go knows; syscall does not
	// export it, so mask the low bits of the speed word the way termios.h
	// spells it.
	const cbaud = 0x100f
	t.Cflag &^= cbaud
	t.Cflag |= speed
	t.Ispeed = speed
	t.Ospeed = speed
	if err := ioctl(f, syscall.TCSETS, unsafe.Pointer(&t)); err != nil {
		return fmt.Errorf("tcsetattr: %w", err)
	}
	return nil
}

// baudSpeeds holds the line speeds linux exposes. B115200 is the getty speed
// everywhere in this project (docs/ARCHITECTURE.md), so it is the default.
var baudSpeeds = map[int]uint32{
	9600: syscall.B9600, 19200: syscall.B19200, 38400: syscall.B38400,
	57600: syscall.B57600, 115200: syscall.B115200, 230400: syscall.B230400,
	460800: syscall.B460800, 921600: syscall.B921600,
}

// waitReadable blocks until f is readable or the timeout expires, and reports
// whether it is readable. A negative timeout blocks without a deadline; a
// zero timeout only polls. It is select(2) because the port is a character
// device: the poller deals with the "/dev/ttyACM0 is not readable" quirk,
// and a select loop on one fd expresses "idle" as "the deadline passed"
// rather than as a read that returned nothing.
func waitReadable(f *os.File, timeout time.Duration) (bool, error) {
	if timeout > 0 {
		tv := syscall.NsecToTimeval(timeout.Nanoseconds())
		_, err := syscall.Select(int(f.Fd())+1, nil, nil, nil, &tv)
		return err == nil, nil
	}
	var set syscall.FdSet
	set.Bits[f.Fd()/64] |= 1 << (uint(f.Fd()) % 64)
	var tv *syscall.Timeval
	if timeout == 0 {
		tv = &syscall.Timeval{}
	}
	n, err := syscall.Select(int(f.Fd())+1, &set, nil, nil, tv)
	if err == syscall.EINTR {
		return false, nil
	}
	if err != nil {
		return false, err
	}
	return n > 0, nil
}

// ptyPair creates a pseudo-terminal pair the way a terminal emulator would:
// open /dev/ptmx, unlock the slave with TIOCSPTLCK, ask for its number with
// TIOCGPTN and open /dev/pts/N. It is only used by the tests, which drive the
// tool against a real shell without hardware.
func ptyPair() (master, slave *os.File, err error) {
	// A double open on the master is what glibc's posix_openpt does to keep
	// the slave alive; a single fd is enough here because the master fd
	// itself is held for the whole session.
	master, err = os.OpenFile("/dev/ptmx", os.O_RDWR|syscall.O_NOCTTY, 0)
	if err != nil {
		return nil, nil, err
	}
	var unlock int32
	if err := ioctl(master, syscall.TIOCSPTLCK, unsafe.Pointer(&unlock)); err != nil {
		master.Close()
		return nil, nil, fmt.Errorf("TIOCSPTLCK: %w", err)
	}
	var n uint32
	if err := ioctl(master, syscall.TIOCGPTN, unsafe.Pointer(&n)); err != nil {
		master.Close()
		return nil, nil, fmt.Errorf("TIOCGPTN: %w", err)
	}
	slave, err = os.OpenFile(fmt.Sprintf("/dev/pts/%d", n), os.O_RDWR|syscall.O_NOCTTY, 0)
	if err != nil {
		master.Close()
		return nil, nil, fmt.Errorf("open slave: %w", err)
	}
	return master, slave, nil
}
