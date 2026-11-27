//go:build !linux

package main

import "fmt"

// remountBoot is only implemented on Linux; the stub keeps non-Linux builds
// compiling and preserves the error wording expected by tests.
func remountBoot(dir string, rw bool) error {
	mode := "ro"
	if rw {
		mode = "rw"
	}
	return fmt.Errorf("mount remount,%s %s: remounting is only implemented on Linux", mode, dir)
}
