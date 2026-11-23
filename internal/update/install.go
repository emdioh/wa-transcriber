package update

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"fmt"
	"io"
	"net/http"
	"os"
	"path/filepath"
	"sync"
	"syscall"
	"time"
)

// partSlack is how far above Asset.Size a download may grow before it is
// refused. It only absorbs a skewed manifest size, not a hostile server.
const partSlack = 64 << 10

// Installer downloads an asset into Dir and records it as the pending slot.
// One Installer may be used from several goroutines: a mutex keeps two
// downloads (and two state updates) from interleaving.
type Installer struct {
	Dir    string
	Client *http.Client
	Now    func() time.Time
	// FreeSpace reports the bytes available in a directory. Nil is statfs.
	// A download that does not fit fails half way through, which looks like a
	// network problem; the check turns it into one sentence before the first
	// byte (a nearly full /data on a small card).
	FreeSpace func(dir string) (int64, error)

	mu sync.Mutex
}

// freeSpace is the request's own probe, or statfs.
func (i *Installer) freeSpace(dir string) (int64, error) {
	if i.FreeSpace != nil {
		return i.FreeSpace(dir)
	}
	var st syscall.Statfs_t
	if err := syscall.Statfs(dir, &st); err != nil {
		return 0, err
	}
	return int64(st.Bavail) * int64(st.Bsize), nil
}

func (i *Installer) now() time.Time {
	if i.Now != nil {
		return i.Now()
	}
	return time.Now()
}

func (i *Installer) client() *http.Client {
	if i.Client != nil {
		return i.Client
	}
	return http.DefaultClient
}

// updateDir is <Dir>/../updates: the transient download directory of
// ARCHITECTURE.md section 11. Downloads are written here instead of /data/bin
// so a boot can delete them without touching a slot.
func (i *Installer) updateDir() string {
	return filepath.Join(filepath.Dir(filepath.Clean(i.Dir)), "updates")
}

// Fetch downloads the asset for assetKey of m, verifies size, sha256 and the
// ELF header, fsyncs it into Dir as wa-transcriber-<version> and makes it the
// pending slot. The state is only touched after the file is complete and
// durable, so no state can ever point at a partial file.
//
// The on-disk order is: .part → write + hash → size/sha/ELF checks → fsync →
// rename into Dir → fsync Dir → state. Any failure removes the .part and
// leaves state.json exactly as it was.
func (i *Installer) Fetch(ctx context.Context, url string, m *Manifest, assetKey string) (Slot, error) {
	var zero Slot
	if m == nil {
		return zero, errors.New("update: fetch without manifest")
	}
	asset, ok := m.Asset(assetKey)
	if !ok {
		return zero, fmt.Errorf("update: release %s has no asset for %s", m.Version, assetKey)
	}
	if !SafeVersion(m.Version) {
		return zero, fmt.Errorf("update: refusing unsafe version %q", m.Version)
	}

	i.mu.Lock()
	defer i.mu.Unlock()

	if err := os.MkdirAll(i.Dir, 0o755); err != nil {
		return zero, fmt.Errorf("update: create %s: %w", i.Dir, err)
	}
	upd := i.updateDir()
	if err := os.MkdirAll(upd, 0o755); err != nil {
		return zero, fmt.Errorf("update: create %s: %w", upd, err)
	}

	// Room for the file before the first byte: the .part is the whole asset.
	if free, err := i.freeSpace(upd); err == nil && free > 0 && free < asset.Size {
		return zero, fmt.Errorf("update: %s needs %d MB and %s has %d MB free",
			asset.File, asset.Size>>20, upd, free>>20)
	}
	// A superseded download from an earlier run is useless: drop it first.
	if parts, err := filepath.Glob(filepath.Join(upd, "*.part")); err == nil {
		for _, p := range parts {
			removeQuiet(p)
		}
	}

	part := filepath.Join(upd, fmt.Sprintf("wa-transcriber-%s.part", m.Version))
	if part, err := i.download(ctx, url, asset, part); err != nil {
		return zero, err
	} else if err := CheckExecutable(part, assetKey); err != nil {
		removeQuiet(part)
		return zero, err
	}

	final := BinaryName(i.Dir, m.Version)
	if err := moveFile(part, final); err != nil {
		removeQuiet(part)
		return zero, err
	}

	st, err := LoadState(i.Dir)
	if err != nil {
		removeQuiet(final)
		return zero, err
	}
	if st.Pending != nil && st.Pending.File != final {
		removeQuiet(st.Pending.File)
	}
	slot := Slot{Version: m.Version, File: final, SHA256: asset.SHA256}
	st.Pending = &slot
	// A new pending version restarts the health gate for it, but the active
	// version keeps whatever it had.
	if err := SaveState(i.Dir, st); err != nil {
		removeQuiet(final)
		return zero, err
	}
	return slot, nil
}

// download streams url into part, hashing as it goes, and returns part after
// verifying length and sha256. The .part is removed on every error.
func (i *Installer) download(ctx context.Context, url string, asset Asset, part string) (string, error) {
	limit := asset.Size + partSlack
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, url, nil)
	if err != nil {
		return "", fmt.Errorf("update: request: %w", err)
	}
	resp, err := i.client().Do(req)
	if err != nil {
		return "", fmt.Errorf("update: download %s: %w", asset.File, err)
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		removeQuiet(part)
		return "", fmt.Errorf("update: download %s: HTTP %s", asset.File, resp.Status)
	}
	if resp.ContentLength > limit {
		removeQuiet(part)
		return "", fmt.Errorf("update: %s is %d bytes, expected %d", asset.File, resp.ContentLength, asset.Size)
	}

	f, err := os.OpenFile(part, os.O_CREATE|os.O_RDWR|os.O_TRUNC|os.O_EXCL, 0o755)
	if err != nil {
		return "", fmt.Errorf("update: create %s: %w", filepath.Base(part), err)
	}
	fail := func(err error) (string, error) {
		f.Close()
		removeQuiet(part)
		return "", err
	}
	h := sha256.New()
	// Read one byte past the cap so an oversized body is detected, not
	// silently truncated into a valid-looking shorbut wrong file.
	n, err := io.Copy(io.MultiWriter(f, h), io.LimitReader(resp.Body, limit+1))
	if err != nil {
		return fail(fmt.Errorf("update: download %s: %w", asset.File, err))
	}
	if n > limit {
		return fail(fmt.Errorf("update: %s is larger than the %d bytes the manifest allows", asset.File, asset.Size))
	}
	if n != asset.Size {
		return fail(fmt.Errorf("update: %s is %d bytes, the manifest says %d", asset.File, n, asset.Size))
	}
	if got := hex.EncodeToString(h.Sum(nil)); got != asset.SHA256 {
		return fail(fmt.Errorf("update: %s has sha256 %s, the manifest says %s", asset.File, got, asset.SHA256))
	}
	if err := f.Chmod(0o755); err != nil {
		return fail(fmt.Errorf("update: chmod %s: %w", filepath.Base(part), err))
	}
	if err := f.Sync(); err != nil {
		return fail(fmt.Errorf("update: fsync %s: %w", filepath.Base(part), err))
	}
	if err := f.Close(); err != nil {
		removeQuiet(part)
		return "", fmt.Errorf("update: close %s: %w", filepath.Base(part), err)
	}
	return part, nil
}

// moveFile renames src to dst (same filesystem: both under the data
// directory) and fsyncs the destination directory.
func moveFile(src, dst string) error {
	if err := os.Rename(src, dst); err != nil {
		return fmt.Errorf("update: rename to %s: %w", filepath.Base(dst), err)
	}
	if err := os.Chmod(dst, 0o755); err != nil {
		return fmt.Errorf("update: chmod %s: %w", filepath.Base(dst), err)
	}
	syncDir(filepath.Dir(dst))
	return nil
}
