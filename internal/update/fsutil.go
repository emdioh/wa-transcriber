package update

import (
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"io"
	"io/fs"
	"log/slog"
	"os"
)

// isNotExist reports whether err is a "file does not exist" error.
func isNotExist(err error) bool { return errors.Is(err, fs.ErrNotExist) }

// Sum returns the lowercase hex sha256 of b, the form manifests and state
// use.
func Sum(b []byte) string {
	h := sha256.Sum256(b)
	return hex.EncodeToString(h[:])
}

// fileSHA returns the sha256 of a file, or an error (including
// fs.ErrNotExist) when it cannot be read.
func fileSHA(path string) (string, error) {
	f, err := os.Open(path)
	if err != nil {
		return "", err
	}
	defer f.Close()
	h := sha256.New()
	if _, err := io.Copy(h, f); err != nil {
		return "", err
	}
	return hex.EncodeToString(h.Sum(nil)), nil
}

// removeQuiet deletes a file whose absence is as good as its deletion: a temp
// file, a superseded slot, junk pruned at boot. It has no error to report
// because every caller is already on a failure path or will prune the same
// file on the next boot; a real problem (a read-only /data) shows up where it
// matters, on the write that failed. It is visible at debug level anyway.
func removeQuiet(path string) {
	if path == "" {
		return
	}
	if err := os.Remove(path); err != nil && !isNotExist(err) {
		slog.Debug("update: cannot remove a stale file", "path", path, "err", err)
	}
}

// slotMatches reports whether the file of s exists and has the recorded
// hash. A slot without a hash (hand-written or adopted state) only needs the
// file to exist.
func slotMatches(s *Slot) bool {
	if s == nil || s.File == "" {
		return false
	}
	if s.SHA256 == "" {
		_, err := os.Stat(s.File)
		return err == nil
	}
	sum, err := fileSHA(s.File)
	if err != nil {
		return false
	}
	return sum == s.SHA256
}
