//go:build !linux

package boot

import "errors"

// The package is a Linux-only idea (PID 1, /proc, block devices). These
// stubs exist so the program still builds on a developer's machine; nothing
// calls Setup there, because IsInit is false.

func mount(string, string, string, uintptr, string) error {
	return errors.New("boot: mounting is only implemented on Linux")
}

func unmount(string, int) error {
	return errors.New("boot: unmounting is only implemented on Linux")
}

func interfaceUp(string) error { return errors.New("boot: interfaceUp is only implemented on Linux") }

func setHostname(string) error { return errors.New("boot: setHostname is only implemented on Linux") }

const (
	msReadOnly = 0
	msNoSuid   = 0
	msNoDev    = 0
	msNoExec   = 0
	msNoAtime  = 0
	msRemount  = 0
)
