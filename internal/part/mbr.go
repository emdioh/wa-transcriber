// Package part provides pure-Go MBR partition table editing and Linux kernel
// partition table notifications (BLKRRPART and BLKPG), replacing sfdisk and
// partx on minimal board images.
package part

import (
	"encoding/binary"
	"errors"
	"fmt"
	"io"
	"math"
	"os"
)

const (
	sectorSize      = 512
	mbrSigOffset    = 510
	partTableOffset = 446
	partEntrySize   = 16
	alignSectors    = 2048 // 1 MiB alignment (2048 * 512 bytes)
)

// mbrEntry holds the fields of one 16-byte primary MBR partition entry.
type mbrEntry struct {
	partType  byte
	startLBA  uint32
	lengthLBA uint32
}

// parseMBREntries validates the 0x55AA MBR signature on a 512-byte sector and
// decodes primary partition entries 1..4.
func parseMBREntries(mbr []byte) ([4]mbrEntry, error) {
	var entries [4]mbrEntry
	if len(mbr) < sectorSize {
		return entries, fmt.Errorf("part: short MBR read (%d bytes)", len(mbr))
	}
	if mbr[mbrSigOffset] != 0x55 || mbr[mbrSigOffset+1] != 0xAA {
		return entries, fmt.Errorf("part: missing MBR signature (got %02x %02x, want 55 aa)",
			mbr[mbrSigOffset], mbr[mbrSigOffset+1])
	}
	for i := range entries {
		off := partTableOffset + i*partEntrySize
		entries[i] = mbrEntry{
			partType:  mbr[off+4],
			startLBA:  binary.LittleEndian.Uint32(mbr[off+8 : off+12]),
			lengthLBA: binary.LittleEndian.Uint32(mbr[off+12 : off+16]),
		}
	}
	return entries, nil
}

// AppendMBRPartition writes a new primary MBR partition entry at partNum (1..4)
// spanning all remaining space after existing primary partitions, aligned up to
// 1 MiB (2048 sectors). It refuses to overwrite an existing partition or a disk
// without a valid MBR signature.
//
// It returns the new partition's byte offset and byte length on disk.
func AppendMBRPartition(diskPath string, partNum int, partType byte) (startByte, lenByte int64, err error) {
	if partNum < 1 || partNum > 4 {
		return 0, 0, fmt.Errorf("part: invalid primary partition number %d (must be 1..4)", partNum)
	}
	if partType == 0 {
		return 0, 0, errors.New("part: partition type must be non-zero")
	}

	f, err := os.OpenFile(diskPath, os.O_RDWR, 0)
	if err != nil {
		return 0, 0, fmt.Errorf("part: opening %s: %w", diskPath, err)
	}
	defer f.Close()

	sizeBytes, err := diskSize(f)
	if err != nil {
		return 0, 0, fmt.Errorf("part: determining size of %s: %w", diskPath, err)
	}
	if sizeBytes < sectorSize {
		return 0, 0, fmt.Errorf("part: %s is smaller than one sector (%d bytes)", diskPath, sizeBytes)
	}

	var mbr [sectorSize]byte
	if err := readFullAt(f, mbr[:], 0); err != nil {
		return 0, 0, fmt.Errorf("part: reading MBR from %s: %w", diskPath, err)
	}
	entries, err := parseMBREntries(mbr[:])
	if err != nil {
		return 0, 0, fmt.Errorf("%s: %w", diskPath, err)
	}

	target := entries[partNum-1]
	if target.partType != 0 || target.lengthLBA != 0 {
		return 0, 0, fmt.Errorf("part: %s partition %d is already occupied (type=0x%02x, len=%d)",
			diskPath, partNum, target.partType, target.lengthLBA)
	}

	var maxEnd uint64
	for _, e := range entries {
		if e.partType == 0 && e.lengthLBA == 0 {
			continue
		}
		end := uint64(e.startLBA) + uint64(e.lengthLBA)
		if end > maxEnd {
			maxEnd = end
		}
	}

	var startLBA uint64
	if maxEnd == 0 {
		startLBA = alignSectors
	} else {
		startLBA = ((maxEnd + alignSectors - 1) / alignSectors) * alignSectors
	}

	totalSectors := uint64(sizeBytes) / sectorSize
	if startLBA >= totalSectors {
		return 0, 0, fmt.Errorf("part: no free space on %s after sector %d (disk has %d sectors)",
			diskPath, maxEnd, totalSectors)
	}
	lengthLBA := totalSectors - startLBA
	if lengthLBA == 0 {
		return 0, 0, fmt.Errorf("part: no free space on %s", diskPath)
	}
	if startLBA > math.MaxUint32 || lengthLBA > math.MaxUint32 {
		return 0, 0, fmt.Errorf("part: partition %d on %s exceeds 32-bit MBR LBA limit (start=%d, len=%d)",
			partNum, diskPath, startLBA, lengthLBA)
	}

	off := partTableOffset + (partNum-1)*partEntrySize
	mbr[off+0] = 0x00 // not bootable
	mbr[off+1] = 0xFE // CHS start (LBA marker)
	mbr[off+2] = 0xFF
	mbr[off+3] = 0xFF
	mbr[off+4] = partType
	mbr[off+5] = 0xFE // CHS end (LBA marker)
	mbr[off+6] = 0xFF
	mbr[off+7] = 0xFF
	binary.LittleEndian.PutUint32(mbr[off+8:off+12], uint32(startLBA))
	binary.LittleEndian.PutUint32(mbr[off+12:off+16], uint32(lengthLBA))

	n, err := f.WriteAt(mbr[:], 0)
	if err != nil {
		return 0, 0, fmt.Errorf("part: writing MBR to %s: %w", diskPath, err)
	}
	if n != sectorSize {
		return 0, 0, fmt.Errorf("part: short MBR write to %s (%d of %d bytes)", diskPath, n, sectorSize)
	}
	if err := f.Sync(); err != nil {
		return 0, 0, fmt.Errorf("part: syncing %s: %w", diskPath, err)
	}
	return int64(startLBA) * sectorSize, int64(lengthLBA) * sectorSize, nil
}

func readFullAt(f *os.File, buf []byte, off int64) error {
	for len(buf) > 0 {
		n, err := f.ReadAt(buf, off)
		buf = buf[n:]
		off += int64(n)
		if err != nil {
			if errors.Is(err, io.EOF) && len(buf) == 0 {
				return nil
			}
			return err
		}
	}
	return nil
}
