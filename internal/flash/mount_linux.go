//go:build linux

package flash

import (
	"fmt"
	"strings"

	"golang.org/x/sys/unix"
)

// mountFS mounts a VFAT boot partition or remounts an existing mount point via
// mount(2). It supports the two call shapes used by OnTargetBoot:
//
//   - mountFS(device, dir, "rw") — normal VFAT mount with nosuid,nodev,noexec
//     (an optional "vfat:" prefix on device is stripped).
//   - mountFS(dir, "", "remount,ro") — remounts dir in place.
func mountFS(src, dir, opts string) error {
	if dir == "" {
		flags := uintptr(unix.MS_REMOUNT | unix.MS_NOSUID | unix.MS_NODEV | unix.MS_NOEXEC)
		for _, tok := range strings.Split(opts, ",") {
			switch strings.TrimSpace(tok) {
			case "ro":
				flags |= unix.MS_RDONLY
			case "rw":
				flags &^= unix.MS_RDONLY
			}
		}
		if err := unix.Mount("", src, "", flags, ""); err != nil {
			return fmt.Errorf("mount -o %s %s: %w", opts, src, err)
		}
		return nil
	}

	dev := src
	fstype := "vfat"
	if rest, ok := strings.CutPrefix(src, "vfat:"); ok {
		dev = rest
	}
	flags := uintptr(unix.MS_NOSUID | unix.MS_NODEV | unix.MS_NOEXEC)
	for _, tok := range strings.Split(opts, ",") {
		switch strings.TrimSpace(tok) {
		case "ro":
			flags |= unix.MS_RDONLY
		case "rw":
			flags &^= unix.MS_RDONLY
		case "remount":
			flags |= unix.MS_REMOUNT
		}
	}
	if err := unix.Mount(dev, dir, fstype, flags, ""); err != nil {
		return fmt.Errorf("mount -t %s -o %s %s %s: %w", fstype, opts, dev, dir, err)
	}
	return nil
}
