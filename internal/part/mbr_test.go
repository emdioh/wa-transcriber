package part

import (
	"bytes"
	"encoding/binary"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

type testEntry struct {
	slot      int // 1..4
	bootable  byte
	partType  byte
	startLBA  uint32
	lengthLBA uint32
}

// makeMBRDisk creates a sparse file of sizeBytes with a valid MBR signature,
// distinctive boot code bytes, and the given primary partition entries.
func makeMBRDisk(t *testing.T, sizeBytes int64, entries []testEntry) (string, [sectorSize]byte) {
	t.Helper()
	path := filepath.Join(t.TempDir(), "disk.img")
	f, err := os.Create(path)
	if err != nil {
		t.Fatal(err)
	}
	defer f.Close()
	if err := f.Truncate(sizeBytes); err != nil {
		t.Fatal(err)
	}
	var mbr [sectorSize]byte
	// Distinctive boot code so we can assert it is never clobbered.
	for i := 0; i < partTableOffset; i++ {
		mbr[i] = byte(i ^ 0xA5)
	}
	for _, e := range entries {
		off := partTableOffset + (e.slot-1)*partEntrySize
		mbr[off+0] = e.bootable
		mbr[off+1], mbr[off+2], mbr[off+3] = 0xFE, 0xFF, 0xFF
		mbr[off+4] = e.partType
		mbr[off+5], mbr[off+6], mbr[off+7] = 0xFE, 0xFF, 0xFF
		binary.LittleEndian.PutUint32(mbr[off+8:off+12], e.startLBA)
		binary.LittleEndian.PutUint32(mbr[off+12:off+16], e.lengthLBA)
	}
	mbr[mbrSigOffset] = 0x55
	mbr[mbrSigOffset+1] = 0xAA
	if _, err := f.WriteAt(mbr[:], 0); err != nil {
		t.Fatal(err)
	}
	return path, mbr
}

func readMBR(t *testing.T, path string) [sectorSize]byte {
	t.Helper()
	f, err := os.Open(path)
	if err != nil {
		t.Fatal(err)
	}
	defer f.Close()
	var mbr [sectorSize]byte
	if err := readFullAt(f, mbr[:], 0); err != nil {
		t.Fatal(err)
	}
	return mbr
}

func TestAppendMBRPartitionAfterThreePartitions(t *testing.T) {
	// 64 MiB disk = 131072 sectors.
	const diskBytes = 64 << 20
	const totalSectors = diskBytes / sectorSize

	// p1: 1 MiB..9 MiB, p2: 9 MiB..25 MiB, p3: ends unaligned at sector 82000
	// so p4 must align up to the next 2048-sector (1 MiB) boundary = 83968.
	path, orig := makeMBRDisk(t, diskBytes, []testEntry{
		{slot: 1, bootable: 0x80, partType: 0x0C, startLBA: 2048, lengthLBA: 16384},
		{slot: 2, bootable: 0x00, partType: 0x83, startLBA: 18432, lengthLBA: 32768},
		{slot: 3, bootable: 0x00, partType: 0x83, startLBA: 51200, lengthLBA: 30800},
	})

	startByte, lenByte, err := AppendMBRPartition(path, 4, 0x83)
	if err != nil {
		t.Fatalf("AppendMBRPartition: %v", err)
	}

	const wantStartLBA uint32 = 83968 // ceil(82000 / 2048) * 2048
	const wantLenLBA uint32 = totalSectors - wantStartLBA
	if startByte != int64(wantStartLBA)*sectorSize {
		t.Fatalf("startByte = %d, want %d", startByte, int64(wantStartLBA)*sectorSize)
	}
	if lenByte != int64(wantLenLBA)*sectorSize {
		t.Fatalf("lenByte = %d, want %d", lenByte, int64(wantLenLBA)*sectorSize)
	}
	if startByte%(alignSectors*sectorSize) != 0 {
		t.Fatalf("startByte %d is not 1 MiB aligned", startByte)
	}

	got := readMBR(t, path)
	// Boot code and p1..p3 must be byte-for-byte untouched.
	p4Off := partTableOffset + 3*partEntrySize
	if !bytes.Equal(got[:p4Off], orig[:p4Off]) {
		t.Fatal("boot code or p1..p3 entries were modified")
	}
	// Signature must be intact.
	if got[mbrSigOffset] != 0x55 || got[mbrSigOffset+1] != 0xAA {
		t.Fatalf("MBR signature = %02x %02x, want 55 aa", got[mbrSigOffset], got[mbrSigOffset+1])
	}
	// Check p4 entry fields.
	p4 := got[p4Off : p4Off+partEntrySize]
	if p4[0] != 0x00 {
		t.Errorf("p4 bootable = 0x%02x, want 0x00", p4[0])
	}
	if p4[1] != 0xFE || p4[2] != 0xFF || p4[3] != 0xFF {
		t.Errorf("p4 CHS start = %02x %02x %02x, want fe ff ff", p4[1], p4[2], p4[3])
	}
	if p4[4] != 0x83 {
		t.Errorf("p4 type = 0x%02x, want 0x83", p4[4])
	}
	if p4[5] != 0xFE || p4[6] != 0xFF || p4[7] != 0xFF {
		t.Errorf("p4 CHS end = %02x %02x %02x, want fe ff ff", p4[5], p4[6], p4[7])
	}
	if gotStart := binary.LittleEndian.Uint32(p4[8:12]); gotStart != wantStartLBA {
		t.Errorf("p4 startLBA = %d, want %d", gotStart, wantStartLBA)
	}
	if gotLen := binary.LittleEndian.Uint32(p4[12:16]); gotLen != wantLenLBA {
		t.Errorf("p4 lengthLBA = %d, want %d", gotLen, wantLenLBA)
	}
}

func TestAppendMBRPartitionAlreadyAligned(t *testing.T) {
	// 1 GiB sparse disk = 2097152 sectors; p3 ends cleanly on a 1 MiB boundary.
	const diskBytes = 1 << 30
	const totalSectors = diskBytes / sectorSize

	path, _ := makeMBRDisk(t, diskBytes, []testEntry{
		{slot: 1, partType: 0x0C, startLBA: 2048, lengthLBA: 65536},    // 32 MiB
		{slot: 2, partType: 0x83, startLBA: 67584, lengthLBA: 327680},  // 160 MiB
		{slot: 3, partType: 0x83, startLBA: 395264, lengthLBA: 327680}, // 160 MiB -> ends at 722944
	})

	startByte, lenByte, err := AppendMBRPartition(path, 4, 0x83)
	if err != nil {
		t.Fatalf("AppendMBRPartition: %v", err)
	}
	const wantStartLBA = 722944
	const wantLenLBA = totalSectors - wantStartLBA
	if startByte != wantStartLBA*sectorSize || lenByte != wantLenLBA*sectorSize {
		t.Fatalf("got (%d, %d), want (%d, %d)",
			startByte, lenByte, wantStartLBA*sectorSize, wantLenLBA*sectorSize)
	}
}

func TestAppendMBRPartitionOnEmptyTable(t *testing.T) {
	const diskBytes = 16 << 20
	path, _ := makeMBRDisk(t, diskBytes, nil)

	startByte, lenByte, err := AppendMBRPartition(path, 1, 0x83)
	if err != nil {
		t.Fatalf("AppendMBRPartition: %v", err)
	}
	if startByte != alignSectors*sectorSize {
		t.Fatalf("startByte = %d, want %d", startByte, alignSectors*sectorSize)
	}
	if lenByte != diskBytes-alignSectors*sectorSize {
		t.Fatalf("lenByte = %d, want %d", lenByte, diskBytes-alignSectors*sectorSize)
	}
}

func TestAppendMBRPartitionRejectsInvalidMBR(t *testing.T) {
	path := filepath.Join(t.TempDir(), "not-mbr.img")
	f, err := os.Create(path)
	if err != nil {
		t.Fatal(err)
	}
	if err := f.Truncate(16 << 20); err != nil {
		t.Fatal(err)
	}
	f.Close()

	if _, _, err := AppendMBRPartition(path, 4, 0x83); err == nil || !strings.Contains(err.Error(), "MBR signature") {
		t.Fatalf("err = %v, want missing MBR signature error", err)
	}
}

func TestAppendMBRPartitionRejectsOccupiedSlot(t *testing.T) {
	path, orig := makeMBRDisk(t, 64<<20, []testEntry{
		{slot: 1, partType: 0x0C, startLBA: 2048, lengthLBA: 16384},
		{slot: 4, partType: 0x83, startLBA: 18432, lengthLBA: 16384},
	})

	if _, _, err := AppendMBRPartition(path, 4, 0x83); err == nil || !strings.Contains(err.Error(), "occupied") {
		t.Fatalf("err = %v, want occupied slot refusal", err)
	}
	if got := readMBR(t, path); got != orig {
		t.Fatal("refused append modified the MBR")
	}
}

func TestAppendMBRPartitionRejectsNoFreeSpace(t *testing.T) {
	const diskBytes = 16 << 20
	const totalSectors = diskBytes / sectorSize

	t.Run("exact end of disk", func(t *testing.T) {
		path, orig := makeMBRDisk(t, diskBytes, []testEntry{
			{slot: 1, partType: 0x0C, startLBA: 2048, lengthLBA: 8192},
			{slot: 2, partType: 0x83, startLBA: 10240, lengthLBA: 8192},
			{slot: 3, partType: 0x83, startLBA: 18432, lengthLBA: totalSectors - 18432},
		})
		if _, _, err := AppendMBRPartition(path, 4, 0x83); err == nil || !strings.Contains(err.Error(), "no free space") {
			t.Fatalf("err = %v, want no free space error", err)
		}
		if got := readMBR(t, path); got != orig {
			t.Fatal("refused append modified the MBR")
		}
	})

	t.Run("less than alignment slack left", func(t *testing.T) {
		path, _ := makeMBRDisk(t, diskBytes, []testEntry{
			{slot: 3, partType: 0x83, startLBA: 2048, lengthLBA: totalSectors - 2048 - 100},
		})
		if _, _, err := AppendMBRPartition(path, 4, 0x83); err == nil || !strings.Contains(err.Error(), "no free space") {
			t.Fatalf("err = %v, want no free space error", err)
		}
	})
}

func TestAppendMBRPartitionRejectsBadArgs(t *testing.T) {
	path, _ := makeMBRDisk(t, 16<<20, nil)
	for _, slot := range []int{-1, 0, 5} {
		if _, _, err := AppendMBRPartition(path, slot, 0x83); err == nil {
			t.Errorf("partNum=%d accepted, want error", slot)
		}
	}
	if _, _, err := AppendMBRPartition(path, 4, 0); err == nil {
		t.Error("partType=0 accepted, want error")
	}
}
