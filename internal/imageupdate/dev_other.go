//go:build !linux

package imageupdate

import "os"

// fsDevice has nothing to report off Linux: the A/B layout is a board thing
// and the rest of the package is still exercised by the tests with injected
// paths.
func fsDevice(os.FileInfo) (uint64, bool) { return 0, false }

// mkdev mirrors the Linux encoding so the tests that run anywhere still
// agree with readDev.
func mkdev(major, minor uint64) uint64 {
	return (minor & 0xff) | (major << 8) | ((minor &^ 0xff) << 12)
}
