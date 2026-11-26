package boot

import (
	"context"
	"errors"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"strings"
	"syscall"
)

const (
	migrateMarkerName = "wat-migrate"
	migrateStagedName = "wat-migrate.d"

	// bootRemountFlags preserves the mount options mountBoot sets on /boot so
	// remount(2) does not clear nosuid,nodev,noexec.
	bootRemountFlags = msNoSuid | msNoDev | msNoExec
)

// migrateFromMarker ports S01data's M11 eMMC migration into init mode.
//
// "Install this card to the internal storage" stages the device's state on the
// target's boot partition (<BootDir>/wat-migrate.d/) and writes
// <BootDir>/wat-migrate last. On the first boot from internal storage (after
// the SD card is removed), mountData creates/mounts /data and then calls this
// function to copy those files into /data atomically and remove the marker.
//
// Power-cut invariants (matching S01data):
//   - Every file is copied to <DataDir>/.<name>.migrating, fsynced, renamed
//     into place, and <DataDir> is fsynced.
//   - Any copy, rename, or unmount failure aborts and leaves the marker intact
//     so the next boot retries idempotently (the source on /boot is read-only).
//   - Only after all files are durable in /data (and any fallback device is
//     unmounted) is <BootDir> remounted rw; the marker is removed BEFORE
//     wat-migrate.d/ so a power cut in between never leaves a dangling marker,
//     then <BootDir> is synced and remounted ro.
//   - Migration failure is never fatal to boot: it logs and returns.
func (o Options) migrateFromMarker(_ context.Context) {
	o = o.withDefaults()

	marker := filepath.Join(o.BootDir, migrateMarkerName)
	fi, err := os.Stat(marker)
	if err != nil || !fi.Mode().IsRegular() {
		return
	}

	stagedDir := filepath.Join(o.BootDir, migrateStagedName)
	if sfi, err := os.Stat(stagedDir); err != nil || !sfi.IsDir() {
		stagedDir = ""
	}

	body, err := o.ReadFile(marker)
	if err != nil {
		o.Log.Warn("cannot read migration marker, keeping it", "path", marker, "err", err)
		return
	}

	from, files := parseMigrateMarker(body)
	if from != "" && !validMigrateDevice(from) {
		o.Log.Warn("wat-migrate names no device, ignoring it", "from", from)
		from = ""
	}

	if stagedDir == "" && from == "" {
		o.Log.Warn("wat-migrate names no staged files and no device, removing it")
		o.cleanupMigrationBoot(marker, "", 0)
		return
	}

	mnt := ""
	if from != "" {
		devPath := filepath.Join(o.DevDir, strings.TrimPrefix(from, "/dev/"))
		if isMigrateBlock(devPath) {
			if err := o.MkdirAll(o.MigrateMountDir, 0o755); err != nil && !errors.Is(err, os.ErrExist) {
				o.Log.Warn("cannot create migration mount point, keeping the marker",
					"dir", o.MigrateMountDir, "err", err)
				return
			}
			flags := uintptr(msReadOnly | msNoDev | msNoSuid | msNoAtime)
			if err := o.Mount(from, o.MigrateMountDir, "ext4", flags, ""); err != nil {
				o.Log.Warn("cannot mount old data partition read-only, keeping the marker",
					"device", from, "err", err)
				_ = os.Remove(o.MigrateMountDir)
				return
			}
			mnt = o.MigrateMountDir
		} else {
			o.Log.Info("wat-migrate names a device that is not present, using staged files", "from", from)
		}
	}

	unmountFrom := func() bool {
		if mnt == "" {
			return true
		}
		if err := o.Unmount(mnt, 0); err != nil {
			o.Log.Warn("unmounting old data partition failed, keeping the marker",
				"target", mnt, "err", err)
			return false
		}
		_ = os.Remove(mnt)
		mnt = ""
		return true
	}

	copied := 0
	for _, name := range files {
		if !isSafeMigrateName(name) {
			o.Log.Warn("wat-migrate lists an unsafe name, skipping it", "name", name)
			continue
		}

		src := ""
		if stagedDir != "" {
			cand := filepath.Join(stagedDir, name)
			if sfi, err := os.Stat(cand); err == nil && sfi.Mode().IsRegular() {
				src = cand
			}
		}
		if src == "" && mnt != "" {
			cand := filepath.Join(mnt, name)
			if mfi, err := os.Stat(cand); err == nil && mfi.Mode().IsRegular() {
				src = cand
			}
		}
		if src == "" {
			continue
		}

		if err := copyFileAtomic(src, o.DataDir, name); err != nil {
			o.Log.Warn("migrating file failed, keeping the marker",
				"file", name, "src", src, "err", err)
			_ = syncDir(o.DataDir)
			unmountFrom()
			return
		}
		copied++
	}

	if err := syncDir(o.DataDir); err != nil {
		o.Log.Warn("syncing data directory failed, keeping the marker",
			"dir", o.DataDir, "err", err)
		unmountFrom()
		return
	}

	if !unmountFrom() {
		return
	}

	o.cleanupMigrationBoot(marker, stagedDir, copied)
}

// cleanupMigrationBoot remounts <BootDir> writable, removes the marker first
// and stagedDir second, syncs, and remounts <BootDir> read-only.
func (o Options) cleanupMigrationBoot(marker, stagedDir string, copied int) {
	if err := o.Mount("", o.BootDir, "", msRemount|bootRemountFlags, ""); err != nil {
		// Nothing is lost: the files are in /data, and keeping the marker only
		// means the next boot copies the same files again through temp names.
		o.Log.Warn("copied files into data partition, but boot partition could not be made writable: keeping the marker",
			"copied", copied, "dir", o.BootDir, "err", err)
		return
	}

	ok := true
	if err := o.removeFile(marker); err != nil && !errors.Is(err, os.ErrNotExist) {
		o.Log.Warn("cannot remove migration marker from boot partition",
			"path", marker, "err", err)
		ok = false
	}
	_ = syncDir(o.BootDir)

	// Only remove the staged directory after the marker itself is gone: if
	// removing the marker failed, the next boot still needs the staged files.
	if ok && stagedDir != "" {
		if err := o.removeAll(stagedDir); err != nil {
			o.Log.Warn("cannot remove staged migration directory from boot partition",
				"dir", stagedDir, "err", err)
			ok = false
		}
		_ = syncDir(o.BootDir)
	}

	if err := o.Mount("", o.BootDir, "", msRemount|msReadOnly|bootRemountFlags, ""); err != nil {
		o.Log.Warn("boot partition is still writable, remounting read-only failed",
			"dir", o.BootDir, "err", err)
	}

	if ok {
		o.Log.Info("migrated files into data partition, removed migration marker", "copied", copied)
	} else {
		o.Log.Warn("migrated files into data partition, but could not remove all migration files from boot partition",
			"copied", copied)
	}
}

// parseMigrateMarker extracts the first `from <dev>` line and every `file <name>`
// line from a wat-migrate body. Validation of device and file names is left to
// the caller so unsafe entries can be logged.
func parseMigrateMarker(body []byte) (from string, files []string) {
	sawFrom := false
	for _, line := range strings.Split(string(body), "\n") {
		line = strings.TrimRight(line, "\r")
		if v, ok := strings.CutPrefix(line, "from "); ok {
			if !sawFrom {
				sawFrom = true
				from = strings.TrimSpace(v)
			}
			continue
		}
		if v, ok := strings.CutPrefix(line, "file "); ok {
			files = append(files, strings.TrimSpace(v))
		}
	}
	return from, files
}

// validMigrateDevice reports whether dev is a clean /dev/... path. The marker
// sits on a FAT partition anyone can edit, so paths outside /dev/ or with
// traversal segments are rejected.
func validMigrateDevice(dev string) bool {
	if !strings.HasPrefix(dev, "/dev/") || len(dev) <= len("/dev/") {
		return false
	}
	return filepath.Clean(dev) == dev
}

// isSafeMigrateName rejects filenames that could escape <DataDir>.
func isSafeMigrateName(name string) bool {
	if name == "" || name == "." || name == ".." {
		return false
	}
	return !strings.ContainsAny(name, "/\\\x00")
}

// isMigrateBlock reports whether path is a block device (or, in a test, a
// regular file that stands in for one), rejecting character devices.
func isMigrateBlock(path string) bool {
	fi, err := os.Stat(path)
	if err != nil {
		return false
	}
	if fi.Mode().IsRegular() {
		return true
	}
	return fi.Mode()&os.ModeDevice != 0 && fi.Mode()&os.ModeCharDevice == 0
}

// copyFileAtomic copies src into <dataDir>/<name> through
// <dataDir>/.<name>.migrating, fsyncing both the temp file and <dataDir>.
func copyFileAtomic(src, dataDir, name string) error {
	in, err := os.Open(src)
	if err != nil {
		return fmt.Errorf("open source: %w", err)
	}
	defer in.Close()

	fi, err := in.Stat()
	if err != nil {
		return fmt.Errorf("stat source: %w", err)
	}
	mode := (fi.Mode().Perm() & 0o666) | 0o600

	tmp := filepath.Join(dataDir, "."+name+".migrating")
	dst := filepath.Join(dataDir, name)
	_ = os.Remove(tmp)

	out, err := os.OpenFile(tmp, os.O_CREATE|os.O_WRONLY|os.O_TRUNC, mode)
	if err != nil {
		return fmt.Errorf("create temp file: %w", err)
	}

	cleanup := func(cause error) error {
		_ = out.Close()
		_ = os.Remove(tmp)
		return cause
	}

	if _, err := io.Copy(out, in); err != nil {
		return cleanup(fmt.Errorf("copy bytes: %w", err))
	}
	if err := out.Sync(); err != nil {
		return cleanup(fmt.Errorf("fsync temp file: %w", err))
	}
	if err := out.Close(); err != nil {
		_ = os.Remove(tmp)
		return fmt.Errorf("close temp file: %w", err)
	}
	if err := os.Rename(tmp, dst); err != nil {
		_ = os.Remove(tmp)
		return fmt.Errorf("rename into place: %w", err)
	}
	if err := syncDir(dataDir); err != nil {
		return fmt.Errorf("fsync data dir: %w", err)
	}
	return nil
}

// syncDir fsyncs a directory so renames and unlinks inside it survive a power
// cut. Filesystems that return EINVAL on directory fsync are tolerated.
func syncDir(dir string) error {
	d, err := os.Open(dir)
	if err != nil {
		return err
	}
	defer d.Close()
	if err := d.Sync(); err != nil {
		if errors.Is(err, syscall.EINVAL) {
			return nil
		}
		return err
	}
	return nil
}
