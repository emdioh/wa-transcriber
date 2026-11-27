//go:build linux

package part

import (
	"fmt"
	"io"
	"math"
	"os"
	"runtime"
	"unsafe"

	"golang.org/x/sys/unix"
)

// diskSize returns the size in bytes of a block device (via BLKGETSIZE64) or
// a regular file standing in for one in tests (via SeekEnd).
func diskSize(f *os.File) (int64, error) {
	var size uint64
	_, _, errno := unix.Syscall(unix.SYS_IOCTL, f.Fd(), unix.BLKGETSIZE64, uintptr(unsafe.Pointer(&size)))
	if errno == 0 {
		if size > math.MaxInt64 {
			return 0, fmt.Errorf("disk size %d overflows int64", size)
		}
		return int64(size), nil
	}
	n, err := f.Seek(0, io.SeekEnd)
	if err != nil {
		return 0, err
	}
	return n, nil
}

// blkpg issues a Linux BLKPG ioctl for a single partition operation.
func blkpg(fd uintptr, op int32, p unix.BlkpgPartition) error {
	arg := unix.BlkpgIoctlArg{
		Op:      op,
		Flags:   0,
		Datalen: int32(unsafe.Sizeof(p)),
		Data:    (*byte)(unsafe.Pointer(&p)),
	}
	_, _, errno := unix.Syscall(unix.SYS_IOCTL, fd, unix.BLKPG, uintptr(unsafe.Pointer(&arg)))
	runtime.KeepAlive(&p)
	runtime.KeepAlive(&arg)
	if errno != 0 {
		return errno
	}
	return nil
}

// AddKernelPartition notifies the running Linux kernel about a single partition
// via BLKPG (BLKPG_ADD_PARTITION), which works even when other partitions on
// the same disk are mounted.
func AddKernelPartition(diskPath string, partNum int, startByte, lenByte int64) error {
	if partNum < 1 {
		return fmt.Errorf("part: invalid partition number %d", partNum)
	}
	if startByte < 0 || lenByte <= 0 {
		return fmt.Errorf("part: invalid partition range start=%d len=%d", startByte, lenByte)
	}
	f, err := os.Open(diskPath)
	if err != nil {
		return fmt.Errorf("part: opening %s: %w", diskPath, err)
	}
	defer f.Close()

	p := unix.BlkpgPartition{
		Start:  startByte,
		Length: lenByte,
		Pno:    int32(partNum),
	}
	if err := blkpg(f.Fd(), unix.BLKPG_ADD_PARTITION, p); err != nil {
		return fmt.Errorf("part: BLKPG add partition %d on %s: %w", partNum, diskPath, err)
	}
	return nil
}

// RereadTable asks the Linux kernel to re-read the partition table of diskPath.
// It tries BLKRRPART first; if the disk has busy partitions (EBUSY), it falls
// back to parsing the MBR entries 1..4 and updating the kernel table through
// BLKPG (DEL then ADD), matching partx -u.
func RereadTable(diskPath string) error {
	f, err := os.Open(diskPath)
	if err != nil {
		return fmt.Errorf("part: opening %s: %w", diskPath, err)
	}
	defer f.Close()

	_, _, errno := unix.Syscall(unix.SYS_IOCTL, f.Fd(), unix.BLKRRPART, 0)
	if errno == 0 {
		return nil
	}
	if errno != unix.EBUSY {
		return fmt.Errorf("part: BLKRRPART %s: %w", diskPath, errno)
	}

	var mbr [sectorSize]byte
	if err := readFullAt(f, mbr[:], 0); err != nil {
		return fmt.Errorf("part: reading MBR from %s: %w", diskPath, err)
	}
	entries, err := parseMBREntries(mbr[:])
	if err != nil {
		return fmt.Errorf("%s: %w", diskPath, err)
	}

	var firstErr error
	for i, e := range entries {
		pno := int32(i + 1)
		derr := blkpg(f.Fd(), unix.BLKPG_DEL_PARTITION, unix.BlkpgPartition{Pno: pno})
		if derr == unix.EBUSY {
			// Partition is currently mounted or open in the kernel; leave it alone.
			continue
		}
		if derr != nil && derr != unix.ENXIO && firstErr == nil {
			firstErr = fmt.Errorf("part: BLKPG del partition %d on %s: %w", pno, diskPath, derr)
			continue
		}
		if e.partType == 0 || e.lengthLBA == 0 {
			continue
		}
		p := unix.BlkpgPartition{
			Start:  int64(e.startLBA) * sectorSize,
			Length: int64(e.lengthLBA) * sectorSize,
			Pno:    pno,
		}
		if aerr := blkpg(f.Fd(), unix.BLKPG_ADD_PARTITION, p); aerr != nil && aerr != unix.EBUSY && firstErr == nil {
			firstErr = fmt.Errorf("part: BLKPG add partition %d on %s: %w", pno, diskPath, aerr)
		}
	}
	return firstErr
}
