package flash

import (
	"errors"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"syscall"
	"time"
)

// Staging the /data migration on the *target's* boot partition.
//
// Why it is staged and not named by device: the marker names the files to
// carry over, and the first boot on the internal storage is the one after the
// owner removed the card (the AM335x ROM boots the SD whenever one is in the
// slot). Naming the card's DATA partition there — the old format — points at
// a device that is not in the slot at that moment, so the copy could never
// happen. The files themselves therefore travel with the clone: they are
// copied into <boot>/wat-migrate.d/ on the target's boot partition, right
// after the read-back, and S01data copies them from there.

// Stage is what the staging left behind, for the report and the console.
type Stage struct {
	// Files are the names that are on the target's boot partition, in
	// MigrateFiles order.
	Files []string
	// Skipped are the MigrateFiles that the running /data does not have (a
	// device without wifi.json, or before the first pairing).
	Skipped []string
}

// ErrNoSpace is staging on a boot partition that is too small for the files.
// It is not a failed clone: the copy is written, verified and bootable, it
// just starts unpaired.
var ErrNoSpace = errors.New("flash: not enough free space on the target's boot partition for the session")

// migrateTempPrefix is how a half-written staged file is named inside
// MigrateDir before its rename: S01data ignores everything that is not in the
// marker, and the prefix is not a safe name, so it can never be listed.
//
// It has to be legal on VFAT, which is what the boot partition is: a colon is
// not, and `open(".staging:config.json")` fails with EINVAL on the board
// (found the hard way — the staging wrote nothing and said "invalid
// argument"). Keep this to letters, digits, dot, dash and underscore.
const migrateTempPrefix = "wat-staging-"

// stageNeed is the bytes the files take on the FAT partition: rounded up to
// whole clusters, because a directory of small files costs more than their
// sum, plus the marker and the directory entries themselves.
func stageNeed(sizes []int64, block int64) int64 {
	if block <= 0 {
		block = 4096
	}
	var n int64 = block // the marker, and the slack of the directory itself
	for _, s := range sizes {
		n += (s + block - 1) / block * block
	}
	return n
}

// stageFiles copies names from dataDir into dir (the target's MigrateDir),
// atomically each: temp name in the same directory, fsync, rename, fsync
// directory. It refuses before writing anything when the journal does not fit
// (ErrNoSpace), and it never fails on a file that /data does not have.
//
// dir must exist and be writable; the caller has made the boot partition so.
func stageFiles(dataDir, dir string, names []string, free, block int64, logf func(string, ...any)) (Stage, error) {
	if logf == nil {
		logf = func(string, ...any) {}
	}
	var st Stage
	type src struct {
		name string
		path string
		size int64
	}
	var found []src
	var sizes []int64
	for _, name := range names {
		if !safeName(name) {
			return st, fmt.Errorf("flash: %q is not a file name to stage", name)
		}
		path := filepath.Join(dataDir, name)
		fi, err := os.Stat(path)
		if err != nil {
			if errors.Is(err, os.ErrNotExist) {
				st.Skipped = append(st.Skipped, name)
				continue
			}
			return st, fmt.Errorf("flash: reading %s: %w", path, err)
		}
		if !fi.Mode().IsRegular() {
			st.Skipped = append(st.Skipped, name)
			continue
		}
		found = append(found, src{name: name, path: path, size: fi.Size()})
		sizes = append(sizes, fi.Size())
	}
	if len(found) == 0 {
		logf("flash: staging: nothing in %s to carry over", dataDir)
		return st, nil
	}
	// The space is checked before the first byte: half a session is worse
	// than none, and the whole thing is retried on the next install anyway.
	need := stageNeed(sizes, block)
	logf("flash: staging: %d file(s), %s needed, %s free on the target's boot partition",
		len(found), sizeMB(need), sizeMB(free))
	if free > 0 && need > free {
		return st, fmt.Errorf("%w: the files need %s, %s free on the target's boot partition",
			ErrNoSpace, sizeMB(stageNeed(sizes, block)), sizeMB(free))
	}
	if err := os.MkdirAll(dir, 0o755); err != nil {
		return st, fmt.Errorf("flash: creating %s: %w", dir, err)
	}
	for _, f := range found {
		started := time.Now()
		logf("flash: staging %s (%s)", f.name, sizeMB(f.size))
		if err := copyAtomic(f.path, filepath.Join(dir, f.name)); err != nil {
			return st, err
		}
		logf("flash: staged %s in %s", f.name, time.Since(started).Round(time.Millisecond))
		st.Files = append(st.Files, f.name)
	}
	// The directory entries, after the files they name.
	if err := syncDir(dir); err != nil {
		return st, err
	}
	return st, nil
}

// copyAtomic copies src to dst through a temp name in dst's directory, so a
// power cut leaves either no dst or the whole file, never half of one.
func copyAtomic(src, dst string) error {
	in, err := os.Open(src)
	if err != nil {
		return fmt.Errorf("flash: opening %s: %w", src, err)
	}
	defer in.Close()
	tmp := filepath.Join(filepath.Dir(dst), migrateTempPrefix+filepath.Base(dst))
	out, err := os.OpenFile(tmp, os.O_WRONLY|os.O_CREATE|os.O_TRUNC, 0o644)
	if err != nil {
		return fmt.Errorf("flash: creating %s: %w", tmp, err)
	}
	fail := func(err error) error {
		out.Close()
		os.Remove(tmp)
		return err
	}
	if _, err := io.Copy(out, in); err != nil {
		return fail(fmt.Errorf("flash: copying %s to the boot partition: %w", src, err))
	}
	if err := out.Sync(); err != nil {
		return fail(fmt.Errorf("flash: fsync %s: %w", tmp, err))
	}
	if err := out.Close(); err != nil {
		os.Remove(tmp)
		return fmt.Errorf("flash: close %s: %w", tmp, err)
	}
	if err := os.Rename(tmp, dst); err != nil {
		os.Remove(tmp)
		return fmt.Errorf("flash: renaming %s into place: %w", dst, err)
	}
	return syncDir(filepath.Dir(dst))
}

// freeSpace is the bytes available to an unprivileged writer in dir, from
// statfs (0 when it cannot be read, which turns the space check off).
func freeSpace(dir string) int64 {
	var s syscall.Statfs_t
	if err := syscall.Statfs(dir, &s); err != nil {
		return 0
	}
	return int64(s.Bavail) * int64(s.Bsize)
}

// blockSize is the allocation unit of the filesystem dir is on (0 when it
// cannot be read).
func blockSize(dir string) int64 {
	var s syscall.Statfs_t
	if err := syscall.Statfs(dir, &s); err != nil {
		return 0
	}
	return int64(s.Bsize)
}
