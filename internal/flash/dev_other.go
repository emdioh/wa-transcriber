//go:build !linux

package flash

import "os"

// fsDevice has no answer off Linux: a dev machine's root is not a disk with
// an internal flash next to it, and stat's device number does not match
// sysfs there. The target rule then finds only "not the running disk" by
// nothing at all, so Target refuses (which is the right answer).
func fsDevice(os.FileInfo) (uint64, bool) { return 0, false }

// mkdev builds the device number stat reports from a major and a minor.
func mkdev(major, minor uint64) uint64 {
	return (minor & 0xff) | (major << 8) | ((minor &^ 0xff) << 12)
}
