package flash

import "syscall"

// unmount detaches a mount point. The flash path is Linux-only (sysfs block
// devices, statfs), so this is the real one; the stub covers the other
// platforms a developer may run the tests on.
func unmount(dir string) error { return syscall.Unmount(dir, 0) }
