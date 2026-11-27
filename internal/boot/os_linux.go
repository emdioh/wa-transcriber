//go:build linux

package boot

import (
	"os"

	"golang.org/x/sys/unix"
)

// mount is mount(2). Remounts pass an empty source and fstype with msRemount.
func mount(source, target, fstype string, flags uintptr, data string) error {
	return unix.Mount(source, target, fstype, flags, data)
}

func unmount(target string, flags int) error {
	return unix.Unmount(target, flags)
}

func setHostname(name string) error { return unix.Sethostname([]byte(name)) }

// openTTY opens a serial or virtual console device without making it PID 1's
// controlling terminal, and configures 115200 8N1 canonical mode when the
// device supports termios.
func openTTY(path string) (*os.File, error) {
	f, err := os.OpenFile(path, os.O_RDWR|unix.O_NOCTTY, 0)
	if err != nil {
		return nil, err
	}
	if tio, err := unix.IoctlGetTermios(int(f.Fd()), unix.TCGETS); err == nil {
		tio.Cflag &^= unix.CSIZE | unix.PARENB | unix.CSTOPB | unix.CBAUD
		tio.Cflag |= unix.CS8 | unix.CREAD | unix.CLOCAL | unix.B115200
		tio.Iflag |= unix.ICRNL
		tio.Oflag |= unix.OPOST | unix.ONLCR
		tio.Lflag |= unix.ICANON | unix.ECHO
		tio.Ispeed = unix.B115200
		tio.Ospeed = unix.B115200
		_ = unix.IoctlSetTermios(int(f.Fd()), unix.TCSETS, tio)
	}
	return f, nil
}

// rebootSystem flushes filesystems and asks the kernel to restart.
func rebootSystem() error {
	unix.Sync()
	return unix.Reboot(unix.LINUX_REBOOT_CMD_RESTART)
}

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
