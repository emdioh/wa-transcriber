package update

import (
	"errors"
	"fmt"
	"io"
	"os"
	"path/filepath"
)

// rollbackDir is <dataDir>/rollback/<version>/.
func rollbackDir(dataDir, version string) (string, error) {
	if !SafeVersion(version) {
		return "", fmt.Errorf("update: refusing unsafe version %q", version)
	}
	return filepath.Join(dataDir, "rollback", version), nil
}

// Snapshot files by name or prefix: config.json plus the SQLite database and
// its WAL and shared-memory sidecars. On a board the WAL can be larger than
// the database (4 MB against 544 KB), so copying only session.db would lose
// the recent messages, the keys, or both.
var snapshotNames = []string{"config.json", "session.db", "session.db-wal", "session.db-shm"}

// Snapshot copies the device's state into <dataDir>/rollback/<version>/ so a
// rollback can restore it. It is meant to run before the first start of a
// new version: whatsmeow migrates session.db in place and an older binary may
// refuse the newer schema.
//
// The caller is expected to stop nothing: the files are read while the
// process may still be writing them, so the copy of session.db is taken
// together with its WAL and is consistent enough to migrate (SQLite replays
// the WAL on open). A missing source file is not an error — a device without
// a WhatsApp session has nothing to save. Returns the directory used.
func Snapshot(dataDir, version string) (string, error) {
	dst, err := rollbackDir(dataDir, version)
	if err != nil {
		return "", err
	}
	if err := os.MkdirAll(dst, 0o700); err != nil {
		return "", fmt.Errorf("update: create snapshot dir: %w", err)
	}
	copied := 0
	for _, name := range snapshotNames {
		src := filepath.Join(dataDir, name)
		if err := copyFile(src, filepath.Join(dst, name), 0o600); err != nil {
			if isNotExist(err) {
				continue
			}
			return "", err
		}
		copied++
	}
	if copied == 0 {
		return dst, nil
	}
	syncDir(dst)
	return dst, nil
}

// Restore copies <dataDir>/rollback/<version>/ back over the live files. The
// caller restarts afterwards; the binary that will run is the one the
// snapshot belongs to. A rollback directory without session.db-wal is fine:
// the WAL is simply absent, which SQLite treats as a fully checkpointed
// database.
func Restore(dataDir, version string) error {
	src, err := rollbackDir(dataDir, version)
	if err != nil {
		return err
	}
	st, err := os.Stat(src)
	if err != nil {
		return fmt.Errorf("update: no snapshot for %s: %w", version, err)
	}
	if !st.IsDir() {
		return fmt.Errorf("update: snapshot %s is not a directory", src)
	}
	restored := 0
	var firstErr error
	for _, name := range snapshotNames {
		from := filepath.Join(src, name)
		if _, err := os.Stat(from); err != nil {
			if isNotExist(err) {
				continue
			}
			if firstErr == nil {
				firstErr = err
			}
			continue
		}
		// SQLite would see the stale sidecars of the newer version next to a
		// restored session.db: remove them before writing the copy.
		if name == "session.db" {
			for _, sidecar := range []string{"session.db-wal", "session.db-shm"} {
				if _, err := os.Stat(filepath.Join(src, sidecar)); isNotExist(err) {
					_ = os.Remove(filepath.Join(dataDir, sidecar))
				}
			}
		}
		if err := copyFile(from, filepath.Join(dataDir, name), 0o600); err != nil {
			if firstErr == nil {
				firstErr = fmt.Errorf("update: restore %s: %w", name, err)
			}
			continue
		}
		restored++
	}
	if firstErr != nil {
		return firstErr
	}
	if restored == 0 {
		return errors.New("update: snapshot is empty")
	}
	syncDir(dataDir)
	return nil
}

// copyFile writes src to dst through a temp file in dst's directory, fsyncs
// and renames, so a power cut never leaves a half-restored config.json. An
// existing destination keeps its mode except for the permission bits.
func copyFile(src, dst string, mode os.FileMode) error {
	in, err := os.Open(src)
	if err != nil {
		return err
	}
	defer in.Close()
	st, err := in.Stat()
	if err != nil {
		return err
	}
	if !st.Mode().IsRegular() {
		return fmt.Errorf("update: %s is not a regular file", filepath.Base(src))
	}
	dir := filepath.Dir(dst)
	if err := os.MkdirAll(dir, 0o700); err != nil {
		return fmt.Errorf("update: create %s: %w", dir, err)
	}
	tmp, err := os.CreateTemp(dir, "."+filepath.Base(dst)+".tmp-*")
	if err != nil {
		return fmt.Errorf("update: create temp copy: %w", err)
	}
	defer os.Remove(tmp.Name())
	if err := tmp.Chmod(mode); err != nil {
		tmp.Close()
		return fmt.Errorf("update: chmod temp copy: %w", err)
	}
	if _, err := io.Copy(tmp, in); err != nil {
		tmp.Close()
		return fmt.Errorf("update: copy %s: %w", filepath.Base(src), err)
	}
	if err := tmp.Sync(); err != nil {
		tmp.Close()
		return fmt.Errorf("update: fsync %s: %w", filepath.Base(dst), err)
	}
	if err := tmp.Close(); err != nil {
		return fmt.Errorf("update: close %s: %w", filepath.Base(dst), err)
	}
	if err := os.Rename(tmp.Name(), dst); err != nil {
		return fmt.Errorf("update: rename %s: %w", filepath.Base(dst), err)
	}
	syncDir(dir)
	return nil
}
