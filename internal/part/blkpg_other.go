//go:build !linux

package part

import (
	"errors"
	"io"
	"os"
)

func diskSize(f *os.File) (int64, error) {
	return f.Seek(0, io.SeekEnd)
}

// AddKernelPartition is only supported on Linux.
func AddKernelPartition(string, int, int64, int64) error {
	return errors.New("part: kernel partition ioctls are only implemented on Linux")
}

// RereadTable is only supported on Linux.
func RereadTable(string) error {
	return errors.New("part: kernel partition ioctls are only implemented on Linux")
}
