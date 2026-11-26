//go:build linux

package boot

import "golang.org/x/sys/unix"

// mount is mount(2). Remounts pass an empty source and fstype with msRemount.
func mount(source, target, fstype string, flags uintptr, data string) error {
	return unix.Mount(source, target, fstype, flags, data)
}

func unmount(target string, flags int) error {
	return unix.Unmount(target, flags)
}

func setHostname(name string) error { return unix.Sethostname([]byte(name)) }

// interfaceUp sets IFF_UP on an interface. The ioctl is enough for lo: no
// address work, no netlink, nothing that can hang against a busy stack.
func interfaceUp(name string) error {
	fd, err := unix.Socket(unix.AF_INET, unix.SOCK_DGRAM, 0)
	if err != nil {
		return err
	}
	defer unix.Close(fd)
	ifr, err := unix.NewIfreq(name)
	if err != nil {
		return err
	}
	if err := unix.IoctlIfreq(fd, unix.SIOCGIFFLAGS, ifr); err != nil {
		return err
	}
	ifr.SetUint32(ifr.Uint32() | unix.IFF_UP)
	return unix.IoctlIfreq(fd, unix.SIOCSIFFLAGS, ifr)
}

// mount(2) flags, named here so the rest of the package needs no build tag.
const (
	msReadOnly = unix.MS_RDONLY
	msNoSuid   = unix.MS_NOSUID
	msNoDev    = unix.MS_NODEV
	msNoExec   = unix.MS_NOEXEC
	msNoAtime  = unix.MS_NOATIME
	msRemount  = unix.MS_REMOUNT
)
