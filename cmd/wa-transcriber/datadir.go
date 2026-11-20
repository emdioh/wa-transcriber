package main

import (
	"fmt"
	"os"
)

// checkWritable fails early when dir cannot hold files. Without this the web
// UI starts and only the first save (usually the setup password) fails, with
// a generic message. The usual cause in Docker is a volume created by an
// older image that ran as root: Docker never changes the ownership of an
// existing volume.
func checkWritable(dir string) error {
	f, err := os.CreateTemp(dir, ".wat-write-test-*")
	if err != nil {
		return err
	}
	name := f.Name()
	if err := f.Close(); err != nil {
		_ = os.Remove(name)
		return err
	}
	return os.Remove(name)
}

// dataDirError wraps checkWritable with the fix for the common Docker case.
func dataDirError(dir string, err error) error {
	return fmt.Errorf("data directory %s is not writable: %w\n"+
		"if it is a Docker volume created by an older version, either recreate it or run:\n"+
		"  docker run --rm -v <volume>:/data busybox chown -R 65532:65532 /data", dir, err)
}
