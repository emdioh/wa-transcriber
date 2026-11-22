package flash

import (
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"strings"
)

// The migration marker: a small file on the boot partition of the *target*
// that tells the first boot on the internal storage where the device's state
// is. The files themselves are staged next to it, in MigrateDir, *on the
// target*: the card they come from is not there any more when that boot
// happens (the AM335x ROM boots the SD whenever one is inserted, so the first
// eMMC boot is the one after the owner took the card out), and a marker that
// named a partition of the card would point at a device that does not exist.
//
// S01data creates and mounts DATA on the new disk, then, if the marker is
// there, copies the staged files from MigrateDir into the new /data before
// deleting the marker and the directory.
//
// A one-line format on purpose — it is read by a shell script in init, on a
// FAT filesystem, before there is a shell to debug it with:
//
//	wat-migrate 1
//	file config.json
//	file session.db
//	file session.db-wal
//	file session.db-shm
//	file wifi.json
//
// The optional "from <device>" line (the old format, still read) names the
// DATA partition of the card instead; its files are copied from there when
// nothing was staged.
const (
	// MarkerName is the file on the boot partition (so /boot/wat-migrate).
	MarkerName = "wat-migrate"
	// MigrateDir is the directory on the same boot partition that holds the
	// staged files, name for name as they are listed (so
	// /boot/wat-migrate.d/config.json).
	MigrateDir = "wat-migrate.d"
	markerHead = "wat-migrate 1"
)

// MigrateFiles are the files that make the device stay itself across the
// install: the settings, the WhatsApp session (SQLite plus its WAL and
// shared-memory sidecar; on a board the WAL is larger than the database, so
// copying only session.db loses the recent messages) and the WiFi networks.
// Same names as update.Snapshot, plus wifi.json, which is not part of the
// binary rollback but is part of an install.
var MigrateFiles = []string{"config.json", "session.db", "session.db-wal", "session.db-shm", "wifi.json"}

// Marker is what /boot/wat-migrate says: which files to carry over to the
// new /data, and, for the old format only, the card's DATA partition.
type Marker struct {
	// From is the device of the old DATA partition, e.g. /dev/mmcblk0p4.
	// Empty when the files are staged on the target's boot partition, which
	// is what an install does: the card is not there on the first eMMC boot.
	From  string
	Files []string
}

// NewMarker builds the marker for a clone from sourceDisk (e.g. mmcblk0):
// DATA is partition 4 of the card layout (D18), and the file list is
// MigrateFiles. The "from" line is informational — the files an install
// stages in MigrateDir are what S01data copies — and empty when the source
// disk cannot be turned into a partition device.
func NewMarker(sourceDisk string) Marker {
	from := ""
	if dev := partitionDev(sourceDisk, 4); dev != "" {
		from = "/dev/" + dev
	}
	return Marker{From: from, Files: append([]string(nil), MigrateFiles...)}
}

// Encode renders the marker in its one-line-per-entry format.
func (m Marker) Encode() []byte {
	var b strings.Builder
	b.WriteString(markerHead + "\n")
	if m.From != "" {
		b.WriteString("from " + m.From + "\n")
	}
	for _, f := range m.Files {
		b.WriteString("file " + f + "\n")
	}
	return []byte(b.String())
}

// WriteMarker writes the marker through the boot partition, made writable for
// the duration by withBootRW (the boot partition is mounted read-only; a nil
// helper means it is already writable, which is what a test does).
//
// It is written last, after the clone has read back clean, and it is written
// atomically (temp file, fsync, rename, fsync directory) like every other
// file whose absence or presence matters after a power cut: a marker that is
// there means "copy /data from what is staged", and it must not be a half
// line. Files it lists that were not staged are copied from the device in its
// "from" line by S01data, so a marker always names at least one source.
func WriteMarker(bootDir string, m Marker, withBootRW func() (func() error, error)) error {
	if bootDir == "" {
		return errors.New("flash: no boot directory for the migration marker")
	}
	if len(m.Files) == 0 {
		return errors.New("flash: the migration marker lists no file to copy")
	}
	unlock := func() error { return nil }
	if withBootRW != nil {
		var err error
		if unlock, err = withBootRW(); err != nil {
			return fmt.Errorf("flash: making %s writable: %w", bootDir, err)
		}
	}
	if unlock == nil {
		unlock = func() error { return nil }
	}
	path := filepath.Join(bootDir, MarkerName)
	werr := writeAtomic(path, m.Encode(), 0o644)
	return errors.Join(werr, unlock())
}

// ReadMarker reads /boot/wat-migrate back (nil when it is not there).
func ReadMarker(bootDir string) (*Marker, error) {
	if bootDir == "" {
		return nil, nil
	}
	b, err := os.ReadFile(filepath.Join(bootDir, MarkerName))
	if err != nil {
		if errors.Is(err, os.ErrNotExist) {
			return nil, nil
		}
		return nil, err
	}
	m, err := parseMarker(string(b))
	if err != nil {
		return nil, err
	}
	return m, nil
}

// parseMarker accepts the format Encode writes and skips anything else, so a
// marker from a newer version degrades to a partial file list instead of a
// boot that stops at init. A "from" line is optional — a marker whose files
// were staged on the boot partition does not need one — but a marker that
// names neither a file nor a source is an error.
func parseMarker(s string) (*Marker, error) {
	m := &Marker{}
	named := 0 // file lines seen, safe or not: a marker with only unsafe names is not "half a line"
	for _, line := range strings.Split(s, "\n") {
		line = strings.TrimSpace(line)
		if line == "" || line == markerHead {
			continue
		}
		if v, ok := strings.CutPrefix(line, "from "); ok {
			m.From = v
			continue
		}
		if v, ok := strings.CutPrefix(line, "file "); ok {
			named++
			if safeName(v) {
				m.Files = append(m.Files, v)
			}
		}
	}
	if named == 0 && m.From == "" {
		return nil, errors.New("flash: " + MarkerName + " names nothing to copy")
	}
	return m, nil
}

// safeName refuses a path in the marker: the init script joins the names to
// /data and to the source mount, and a marker is a file on a FAT partition
// that anyone with a card reader can write.
func safeName(v string) bool {
	if v == "" || strings.ContainsAny(v, "/\\") || v == "." || v == ".." {
		return false
	}
	for i := 0; i < len(v); i++ {
		c := v[i]
		if !(c >= 'a' && c <= 'z' || c >= 'A' && c <= 'Z' || c >= '0' && c <= '9' || c == '.' || c == '-' || c == '_') {
			return false
		}
	}
	return true
}

// writeAtomic writes b to path through a temp file in the same directory, so
// a power cut leaves either the old file or the new one, never half of one.
func writeAtomic(path string, b []byte, mode os.FileMode) error {
	dir := filepath.Dir(path)
	tmp, err := os.CreateTemp(dir, "."+filepath.Base(path)+".*")
	if err != nil {
		return fmt.Errorf("flash: create temp marker: %w", err)
	}
	name := tmp.Name()
	fail := func(err error) error {
		tmp.Close()
		os.Remove(name)
		return err
	}
	if _, err := tmp.Write(b); err != nil {
		return fail(fmt.Errorf("flash: write temp marker: %w", err))
	}
	if err := tmp.Chmod(mode); err != nil {
		return fail(fmt.Errorf("flash: chmod temp marker: %w", err))
	}
	if err := tmp.Sync(); err != nil {
		return fail(fmt.Errorf("flash: fsync temp marker: %w", err))
	}
	if err := tmp.Close(); err != nil {
		os.Remove(name)
		return fmt.Errorf("flash: close temp marker: %w", err)
	}
	if err := os.Rename(name, path); err != nil {
		os.Remove(name)
		return fmt.Errorf("flash: rename temp marker: %w", err)
	}
	return syncDir(dir)
}

// syncDir fsyncs a directory so a rename inside it survives a power cut. A
// filesystem that does not do it (some FAT setups return EINVAL) is not an
// error worth failing on: the file itself is already fsynced.
func syncDir(dir string) error {
	d, err := os.Open(dir)
	if err != nil {
		return nil
	}
	defer d.Close()
	if err := d.Sync(); err != nil {
		return fmt.Errorf("flash: fsync %s: %w", dir, err)
	}
	return nil
}
