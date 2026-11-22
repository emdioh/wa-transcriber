package flash

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"fmt"
	"io"
)

// chunkSize is how much is read, written and hashed at a time. 1 MiB is
// small enough that a power cut loses at most one chunk and that a 128 MB
// board keeps its heap flat, and big enough that a 4 GB clone is not a
// million I/O calls.
const chunkSize = 1 << 20

// Region is one piece of the source to copy: the MBR (the partition table the
// bootloader reads) and each system partition, by offset and length.
//
// Offsets are absolute in the source and in the destination: a clone writes
// each region where it already is on the card, so the partition table it
// copied at offset 0 is true for the eMMC too.
type Region struct {
	// Name is what a failure names: "mbr", "boot", "root-a", "root-b".
	Name   string
	Offset int64
	Size   int64
	// SHA256 is the hash of the source as the clone read it, and the hash
	// the destination has to read back with. Clone fills it in; Verify needs
	// it and refuses a region without one.
	SHA256 string
}

func (r Region) String() string {
	return fmt.Sprintf("%s (%s at %d)", r.Name, sizeMB(r.Size), r.Offset)
}

// ErrNotWritten is the "this is not bootable yet" error: the region named in
// it was not written or did not read back, so the target holds a copy that is
// incomplete or wrong. The source is untouched.
type ErrNotWritten struct {
	Region string
	Err    error
}

func (e *ErrNotWritten) Error() string {
	return fmt.Sprintf("flash: %s was not written: %v", e.Region, e.Err)
}

func (e *ErrNotWritten) Unwrap() error { return e.Err }

// Regions lays out the parts of the running card a clone copies: the MBR plus
// the three system partitions, from their sizes. Each partition starts where
// the previous one ended; between the end of a partition and the start of the
// next one nothing is copied, but every offset is still part of the hash, so
// a source that shifts by a sector is caught.
//
// The MBR (the first 512 bytes) carries the DOS partition table the
// bootloader reads, so it is region zero, padded to one sector. Nothing else
// in the first MiB is needed: on the BeagleBone Black the SD/eMMC boot data
// is not where the ROM looks for it (the ROM reads MLO from the FAT boot
// partition), and on the Pi 2 bootcode.bin/start.elf come from the FAT
// partition as well.
func Regions(mbr, boot, rootA, rootB int64) []Region {
	const sector = 512
	if mbr <= 0 || mbr > sector {
		mbr = sector
	}
	out := []Region{{Name: "mbr", Offset: 0, Size: mbr}}
	at := mbr
	for _, p := range []struct {
		name string
		size int64
	}{{"boot", boot}, {"root-a", rootA}, {"root-b", rootB}} {
		if p.size <= 0 {
			return nil
		}
		out = append(out, Region{Name: p.name, Offset: at, Size: p.size})
		at += p.size
	}
	return out
}

// Need is the number of bytes Regions covers: the smallest target that can
// hold the clone, which is what Target checks before anything is written.
func Need(regions []Region) int64 {
	var n int64
	for _, r := range regions {
		if end := r.Offset + r.Size; end > n {
			n = end
		}
	}
	return n
}

// checkRegions refuses a region list that is empty, unnamed, out of order or
// overlapping: a clone with a hole in it is a target that boots and then
// half-works.
func checkRegions(regions []Region) error {
	if len(regions) == 0 {
		return errors.New("flash: nothing to copy")
	}
	var end int64 = -1
	for _, r := range regions {
		if r.Name == "" {
			return errors.New("flash: a region has no name")
		}
		if r.Size <= 0 {
			return fmt.Errorf("flash: region %s has size %d", r.Name, r.Size)
		}
		if r.Offset < end {
			return fmt.Errorf("flash: region %s starts at %d, inside the region before it (ends at %d)",
				r.Name, r.Offset, end)
		}
		if r.Offset < 0 {
			return fmt.Errorf("flash: region %s has offset %d", r.Name, r.Offset)
		}
		end = r.Offset + r.Size
	}
	return nil
}

// Clone copies the regions from src to dst, hashing every region while it is
// read and again while it is written, then reads the destination back (Verify)
// and compares both hashes with the ones just computed.
//
// It returns the regions with their SHA256 filled in, and only when every
// byte read back equal to the source did. A cancelled or failed clone returns
// an *ErrNotWritten naming the region that was not written (or not verified):
// nothing reports success for a partial copy. progress, when not nil, is
// called after each chunk with the bytes done and the total; reads account
// for half of the total each, so the bar reaches the end only after the
// read-back.
//
// dst is the whole target disk: the regions are written at their absolute
// offsets and the rest of the disk is left as it was, which is what lets a
// 4 GB eMMC take a 400 MB card.
func Clone(ctx context.Context, src io.ReaderAt, dst io.WriterAt, regions []Region,
	progress func(done, total int64)) ([]Region, error) {
	if err := checkRegions(regions); err != nil {
		return nil, err
	}
	if src == nil || dst == nil {
		return nil, errors.New("flash: clone needs a source and a destination")
	}
	rb, ok := dst.(io.ReaderAt)
	if !ok {
		return nil, errors.New("flash: the destination cannot be read back, so the copy could not be verified")
	}
	out := make([]Region, len(regions))
	copy(out, regions)
	total := Need(out)
	var done int64
	buf := make([]byte, chunkSize)
	for i := range out {
		r := &out[i]
		h := sha256.New()
		for off := int64(0); off < r.Size; off += chunkSize {
			if err := ctx.Err(); err != nil {
				return nil, &ErrNotWritten{Region: r.String(), Err: err}
			}
			n, err := readAtFull(src, r.Offset+off, buf[:min(int64(len(buf)), r.Size-off)])
			if err != nil {
				return nil, &ErrNotWritten{Region: r.String(), Err: fmt.Errorf("reading the source: %w", err)}
			}
			h.Write(buf[:n])
			if _, err := dst.WriteAt(buf[:n], r.Offset+off); err != nil {
				return nil, &ErrNotWritten{Region: r.String(), Err: fmt.Errorf("writing the target: %w", err)}
			}
			done += int64(n)
			if progress != nil {
				progress(done, total)
			}
		}
		r.SHA256 = hex.EncodeToString(h.Sum(nil))
	}
	// The write is only half of it: an eMMC that silently wrote different
	// bytes (or a cache that never reached the medium) is caught by reading
	// it back, and this is the one check that separates "the target has a
	// copy" from "the target will boot". A failure here keeps the hashes:
	// they are what a second Verify, or a report, needs.
	if err := Verify(ctx, src, rb, out, progress); err != nil {
		return out, err
	}
	return out, nil
}

// Verify re-reads dst and compares it with the source the regions were copied
// from: a region whose two hashes differ is an *ErrNotWritten, so a caller can
// answer "the internal storage is not bootable yet" and name the piece.
//
// Both sides are re-read, not just the destination, because that is what makes
// the answer worth having: if the source changed while it was cloned (the
// running root is written to, /data is not copied but sits on the same disk)
// the two hashes disagree as well, and the clone is repeated. Regions are
// expected to carry the hashes Clone computed.
func Verify(ctx context.Context, src io.ReaderAt, dst io.ReaderAt, regions []Region,
	progress func(done, total int64)) error {
	if err := checkRegions(regions); err != nil {
		return err
	}
	if src == nil || dst == nil {
		return errors.New("flash: verify needs a source and a destination")
	}
	buf := make([]byte, chunkSize)
	total := 2 * Need(regions) // read and written
	done := Need(regions)
	if progress != nil {
		progress(done, total)
	}
	for _, r := range regions {
		want := r.SHA256
		if want == "" {
			return &ErrNotWritten{Region: r.String(), Err: errors.New("the region was never hashed")}
		}
		gotS, err := hashAt(ctx, src, r, buf)
		if err != nil {
			return &ErrNotWritten{Region: r.String(), Err: fmt.Errorf("reading the source: %w", err)}
		}
		gotD, err := hashAt(ctx, dst, r, buf)
		if err != nil {
			return &ErrNotWritten{Region: r.String(), Err: fmt.Errorf("reading the target back: %w", err)}
		}
		if gotS != want {
			return &ErrNotWritten{Region: r.String(), Err: fmt.Errorf("the source changed while it was copied (sha256 %s, was %s)",
				gotS, want)}
		}
		if gotD != want {
			return &ErrNotWritten{Region: r.String(), Err: fmt.Errorf("the target reads back with sha256 %s, expected %s", gotD, want)}
		}
		done += r.Size
		if progress != nil {
			progress(done, total)
		}
	}
	return nil
}

// hashAt hashes one region of a reader.
func hashAt(ctx context.Context, dev io.ReaderAt, r Region, buf []byte) (string, error) {
	h := sha256.New()
	for off := int64(0); off < r.Size; off += chunkSize {
		if err := ctx.Err(); err != nil {
			return "", err
		}
		n, err := readAtFull(dev, r.Offset+off, buf[:min(int64(len(buf)), r.Size-off)])
		if err != nil {
			return "", err
		}
		h.Write(buf[:n])
	}
	return hex.EncodeToString(h.Sum(nil)), nil
}

// readAtFull reads until p is full or the device ends. A ReaderAt that
// returns a short read with no error is legal (a block device at the end of
// the medium), so the loop is what turns it into a concrete byte count.
func readAtFull(r io.ReaderAt, off int64, p []byte) (int, error) {
	n := 0
	for n < len(p) {
		m, err := r.ReadAt(p[n:], off+int64(n))
		n += m
		if err != nil {
			if errors.Is(err, io.EOF) {
				return n, fmt.Errorf("unexpected end of device at offset %d: %w", off+int64(n), io.ErrUnexpectedEOF)
			}
			return n, err
		}
		if m == 0 {
			return n, fmt.Errorf("read of %d bytes at offset %d returned nothing", len(p)-n, off+int64(n))
		}
	}
	return n, nil
}
