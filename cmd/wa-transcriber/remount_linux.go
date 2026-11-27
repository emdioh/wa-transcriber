//go:build linux

package main

import (
	"fmt"

	"golang.org/x/sys/unix"
)

// remountBoot enters (rw) or leaves (ro) the boot partition via mount(2).
// The boot partition is read-only by design, so it is writable only for the
// millisecond of one atomic rename.
func remountBoot(dir string, rw bool) error {
	mode := "ro"
	flags := uintptr(unix.MS_REMOUNT | unix.MS_NOSUID | unix.MS_NODEV | unix.MS_NOEXEC)
	if rw {
		mode = "rw"
	} else {
		flags |= unix.MS_RDONLY
	}
	if err := unix.Mount("", dir, "", flags, ""); err != nil {
		return fmt.Errorf("mount remount,%s %s: %w", mode, dir, err)
	}
	return nil
}
