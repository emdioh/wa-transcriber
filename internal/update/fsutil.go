package update

import (
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"io"
	"io/fs"
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
