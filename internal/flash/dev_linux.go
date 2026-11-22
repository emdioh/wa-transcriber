//go:build linux

package flash

import (
	"os"
	"syscall"
)

// fsDevice returns the device number of the filesystem fi belongs to: the
// number stat("/") and /sys/class/block/*/dev agree on.
func fsDevice(fi os.FileInfo) (uint64, bool) {
	st, ok := fi.Sys().(*syscall.Stat_t)
	if !ok {
		return 0, false
	}
	return uint64(st.Dev), true // Dev is int32 on some arches, uint64 on others
}

// mkdev builds the device number stat reports from a major and a minor, with
// the same layout the kernel uses (glibc's makedev).
func mkdev(major, minor uint64) uint64 {
	return (minor & 0xff) | (major << 8) | ((minor &^ 0xff) << 12)
}
