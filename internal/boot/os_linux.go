//go:build linux

package boot

import "golang.org/x/sys/unix"

// mount is mount(2). The zero-filesystem case (a bind mount) does not occur
// here: every call names its type.
func mount(source, target, fstype string, flags uintptr, data string) error {
	return unix.Mount(source, target, fstype, flags, data)
}

func setHostname(name string) error { return unix.Sethostname([]byte(name)) }

// mount(2) flags, named here so the rest of the package needs no build tag.
const (
	msReadOnly = unix.MS_RDONLY
	msNoSuid   = unix.MS_NOSUID
	msNoDev    = unix.MS_NODEV
	msNoExec   = unix.MS_NOEXEC
	msNoAtime  = unix.MS_NOATIME
)
