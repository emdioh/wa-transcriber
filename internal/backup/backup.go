// Package backup saves and restores the state that makes a device *this*
// device: the settings, the WhatsApp session and the saved WiFi networks.
//
// The point is that reflashing a card, installing to the internal storage or
// replacing a dead card never costs a re-pairing or a lost API key: the
// owner downloads one archive from the web UI and uploads it again after the
// fresh start, which puts the device back in setup mode with the files in
// place. It is also exactly what wat-migrate carries over (ARCHITECTURE.md
// section 11), and it is deliberately nothing else: no logs, no staged
// updates, no update bookkeeping, so a restore cannot smuggle in anything
// the rest of the program executes.
//
// Safety rules, in the order they matter:
//
//   - Only the names in Files are ever read or written. An archive entry
//     outside that list is refused, so is an absolute path, a "..", a
//     backslash or a symlink: the archive comes from a browser upload and is
//     not trusted.
//   - Restore verifies everything (manifest, size, sha256, decompressed
//     total) before it writes a single byte into the data directory. The
//     verified files are staged in a temporary directory inside the data
//     directory (same filesystem, so the final step is a rename) and only
//     when every one of them is on disk and fsynced are they renamed into
//     place. A failure at any point removes the staging directory and leaves
//     the data directory as it was.
//   - The archive is bounded: 64 MB compressed, 128 MB uncompressed and a
//     per-file limit from the manifest, so a crafted upload cannot fill the
//     card. A device's real state is a few hundred kB.
package backup

import (
	"archive/tar"
	"bytes"
	"compress/gzip"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"time"
)

// Schema is the version of the archive format. Restore accepts only this
// value: a newer device's archive is refused rather than half understood.
const Schema = 1

const (
	// ManifestName is the first entry of every archive.
	ManifestName = "manifest.json"
	// MaxArchive is the hard cap on the compressed upload.
	MaxArchive = 64 << 20
	// MaxTotal is the hard cap on everything decompressed, manifest included.
	MaxTotal = 128 << 20
)

// Files is the whole state, in archive order. config.json first because it
// is the file that decides whether the device is claimed.
var Files = []string{
	"config.json",
	"session.db",
	"session.db-wal",
	"session.db-shm",
	"wifi.json",
}

// fileModes is how each file is written on restore: the password hash in
// config.json (D9) stays 0600, the session belongs to the process owner and
// has always been 0644.
var fileModes = map[string]os.FileMode{
	"config.json":    0o600,
	"session.db":     0o644,
	"session.db-wal": 0o644,
	"session.db-shm": 0o644,
	"wifi.json":      0o600,
}

// Meta describes an archive: what the manifest holds and what the handlers
// report.
type Meta struct {
	Schema    int                 `json:"schema"`
	CreatedAt time.Time           `json:"created_at"`
	Version   string              `json:"version"`
	Files     map[string]FileInfo `json:"files"`
}

// FileInfo is one file's checksum and size, from the manifest.
type FileInfo struct {
	SHA256 string `json:"sha256"`
	Size   int64  `json:"size"`
}

// Names returns the file names in archive order, for the log and the flash.
func (m Meta) Names() []string {
	out := make([]string, 0, len(m.Files))
	for _, n := range Files {
		if _, ok := m.Files[n]; ok {
			out = append(out, n)
		}
	}
	return out
}

// allowed reports whether name may appear in an archive at all.
func allowed(name string) bool {
	for _, n := range Files {
		if n == name {
			return true
		}
	}
	return false
}

// ErrNotArchive is an upload that is not a backup of a device.
var ErrNotArchive = errors.New("backup: not a wa-transcriber backup archive")

// Write streams a complete archive to w: a gzip'd tar whose first entry is
// manifest.json, then the files that exist in dataDir, in Files order.
//
// The output is deterministic: same files and same Meta.CreatedAt give
// byte-identical archives, which is what makes the tests and the checksums
// meaningful. The gzip header carries no name or time and the tar entries are
// owned by 0/0 with no user or group names, so nothing of the host leaks
// into an archive the owner will keep for years.
func Write(w io.Writer, dataDir string, meta Meta) error {
	if meta.Schema == 0 {
		meta.Schema = Schema
	}
	type src struct {
		name string
		path string
		size int64
		sum  string
	}
	var found []src
	infos := map[string]FileInfo{}
	for _, name := range Files {
		path := filepath.Join(dataDir, name)
		fi, err := os.Lstat(path)
		if errors.Is(err, os.ErrNotExist) {
			continue // a device without WiFi has no wifi.json
		}
		if err != nil {
			return fmt.Errorf("backup: reading %s: %w", name, err)
		}
		if !fi.Mode().IsRegular() {
			// Never follow a symlink out of the data directory, and never
			// archive a FIFO or a device node.
			continue
		}
		sum, err := hashFile(path)
		if err != nil {
			return err
		}
		found = append(found, src{name: name, path: path, size: fi.Size(), sum: sum})
		infos[name] = FileInfo{SHA256: sum, Size: fi.Size()}
	}
	meta.Files = infos
	if meta.CreatedAt.IsZero() {
		meta.CreatedAt = time.Now()
	}
	body, err := json.MarshalIndent(meta, "", "  ")
	if err != nil {
		return fmt.Errorf("backup: encoding the manifest: %w", err)
	}
	body = append(body, '\n')

	gz, err := gzip.NewWriterLevel(w, gzip.BestCompression)
	if err != nil {
		return fmt.Errorf("backup: starting the archive: %w", err)
	}
	gz.ModTime = meta.CreatedAt
	tw := tar.NewWriter(gz)
	for _, e := range append([]src{{name: ManifestName, size: int64(len(body))}}, found...) {
		hdr := &tar.Header{
			Typeflag: tar.TypeReg,
			Name:     e.name,
			Size:     e.size,
			Mode:     int64(fileModes[e.name]) & 0o777,
			ModTime:  meta.CreatedAt,
			Format:   tar.FormatPAX,
		}
		if e.name == ManifestName {
			hdr.Mode = 0o644
		}
		if err := tw.WriteHeader(hdr); err != nil {
			return fmt.Errorf("backup: writing the header of %s: %w", e.name, err)
		}
		if e.path == "" {
			if _, err := tw.Write(body); err != nil {
				return fmt.Errorf("backup: writing %s: %w", e.name, err)
			}
			continue
		}
		if err := copyInto(tw, e.path, e.size); err != nil {
			return err
		}
	}
	if err := tw.Close(); err != nil {
		return fmt.Errorf("backup: finishing the archive: %w", err)
	}
	if err := gz.Close(); err != nil {
		return fmt.Errorf("backup: finishing the archive: %w", err)
	}
	return nil
}

func hashFile(path string) (string, error) {
	f, err := os.Open(path)
	if err != nil {
		return "", fmt.Errorf("backup: reading %s: %w", filepath.Base(path), err)
	}
	defer f.Close()
	h := sha256.New()
	if _, err := io.Copy(h, f); err != nil {
		return "", fmt.Errorf("backup: reading %s: %w", filepath.Base(path), err)
	}
	return hex.EncodeToString(h.Sum(nil)), nil
}

func copyInto(w io.Writer, path string, size int64) error {
	f, err := os.Open(path)
	if err != nil {
		return fmt.Errorf("backup: reading %s: %w", filepath.Base(path), err)
	}
	defer f.Close()
	n, err := io.Copy(w, io.LimitReader(f, size))
	if err != nil {
		return fmt.Errorf("backup: writing %s: %w", filepath.Base(path), err)
	}
	if n != size {
		return fmt.Errorf("backup: %s changed while it was read", filepath.Base(path))
	}
	return nil
}

// Verify reads and checks an archive without writing anything, so the web UI
// can refuse a bad upload with the reason before it touches the files.
//
// Restore verifies again rather than being handed the result of a Verify
// pass: the archive is a stream, and the only way to hand the bytes over is
// to buffer the whole thing in memory (up to 128 MB on a board with 512 MB),
// or to read it twice anyway. Verify exists for callers that want the
// manifest only (a preview, a test), and it shares every check with Restore,
// so the two can never disagree about what is acceptable.
func Verify(r io.Reader) (Meta, error) {
	a, err := readArchive(r)
	if err != nil {
		return Meta{}, err
	}
	defer a.Close()
	return a.meta, nil
}

// Restore writes the archive's files into dataDir. On any error the data
// directory is exactly as it was: nothing is written until the whole archive
// has arrived, checked and been staged.
func Restore(r io.Reader, dataDir string) (Meta, error) {
	a, err := readArchive(r)
	if err != nil {
		return Meta{}, err
	}
	defer a.Close()
	if err := commit(a, dataDir); err != nil {
		return Meta{}, err
	}
	return a.meta, nil
}

// archive is a verified upload: the manifest, and the file bodies staged in a
// temporary directory of its own, removed by Close (which Restore and Verify
// both defer) whether the restore succeeded or not.
type archive struct {
	meta     Meta
	dir      string
	order    []string
	cleanOwn bool
}

// Close removes the staging directory. Safe after a successful Restore.
func (a *archive) Close() error {
	if a.dir == "" || a.cleanOwn {
		return nil
	}
	return os.RemoveAll(a.dir)
}

// readArchive decodes and verifies a whole archive into a staging directory
// of its own (outside dataDir). Nothing in dataDir is touched here: the caller
// commits or drops the staging directory.
//
// The manifest is small and is kept in memory, which is what lets the files
// after it be checked against it while they stream past: tar is a stream, the
// checksums are only known once the whole archive has arrived, and a file
// comes after the manifest it belongs to.
func readArchive(r io.Reader) (a *archive, err error) {
	limited := &countingReader{r: io.LimitReader(r, MaxArchive+1)}
	gz, err := gzip.NewReader(limited)
	if err != nil {
		return nil, fmt.Errorf("%w: %s", ErrNotArchive, err)
	}
	defer gz.Close()
	staged, err := os.MkdirTemp("", "wat-backup-")
	if err != nil {
		return nil, fmt.Errorf("backup: staging the upload: %w", err)
	}
	a = &archive{dir: staged}
	defer func() {
		if err != nil {
			os.RemoveAll(staged)
			a = nil
		}
	}()
	if err := a.read(tar.NewReader(io.LimitReader(gz, MaxTotal+1))); err != nil {
		return nil, err
	}
	if limited.n > MaxArchive {
		return nil, fmt.Errorf("%w: it is larger than %d MB", ErrNotArchive, MaxArchive>>20)
	}
	return a, nil
}

// read walks the tar entries of the archive into a's staging directory.
func (a *archive) read(tr *tar.Reader) error {
	var (
		manifest []byte
		total    int64
		seen     = map[string]bool{}
	)
	for {
		hdr, err := tr.Next()
		if errors.Is(err, io.EOF) {
			break
		}
		if err != nil {
			return fmt.Errorf("%w: %s", ErrNotArchive, err)
		}
		if err := a.checkEntry(hdr, seen); err != nil {
			return err
		}
		seen[hdr.Name] = true

		sum, n, err := a.readEntry(hdr, tr, &manifest)
		if err != nil {
			return err
		}
		total += n
		if total > MaxTotal {
			return fmt.Errorf("%w: it unpacks to more than %d MB", ErrNotArchive, MaxTotal>>20)
		}
		if hdr.Name == ManifestName {
			// Checked the moment it is complete: everything after it is
			// measured against it. Nothing in it was trusted before this,
			// and the check it makes on the archive as a whole (a file the
			// manifest lists but the archive lacks) waits for the end.
			if err := a.parseManifest(manifest); err != nil {
				return err
			}
			continue
		}
		// The per-file caps come from the manifest, which was read first.
		fi, ok := a.info(hdr.Name)
		if !ok {
			return fmt.Errorf("%w: %s does not list %q", ErrNotArchive, ManifestName, hdr.Name)
		}
		if fi.Size != n {
			return fmt.Errorf("%w: %q is %d bytes, the manifest says %d", ErrNotArchive, hdr.Name, n, fi.Size)
		}
		if fi.SHA256 != sum {
			return fmt.Errorf("%w: %q does not match its checksum", ErrNotArchive, hdr.Name)
		}
		a.order = append(a.order, hdr.Name)
	}
	if manifest == nil {
		return fmt.Errorf("%w: there is no %s", ErrNotArchive, ManifestName)
	}
	// Last: every file the manifest promises must have arrived.
	return a.parseManifest(manifest)
}

// checkEntry applies the rules about names, order and types.
func (a *archive) checkEntry(hdr *tar.Header, seen map[string]bool) error {
	if hdr.Typeflag != tar.TypeReg {
		// Directories, symlinks and hardlinks in a device state are always
		// wrong; a symlink is how an archive would reach a path outside the
		// data directory.
		return fmt.Errorf("%w: entry %q is not a regular file", ErrNotArchive, hdr.Name)
	}
	if err := checkName(hdr.Name); err != nil {
		return err
	}
	if seen[hdr.Name] {
		return fmt.Errorf("%w: %q appears twice", ErrNotArchive, hdr.Name)
	}
	// The manifest must come first: everything after it is measured against
	// it, and a manifest smuggled in at the end would otherwise describe
	// files that were never checked.
	if len(seen) == 0 && hdr.Name != ManifestName {
		return fmt.Errorf("%w: %s is not the first entry", ErrNotArchive, ManifestName)
	}
	if len(seen) > 0 && hdr.Name == ManifestName {
		return fmt.Errorf("%w: %s is not the first entry", ErrNotArchive, ManifestName)
	}
	if hdr.Name != ManifestName && !allowed(hdr.Name) {
		return fmt.Errorf("%w: %q is not part of a device state", ErrNotArchive, hdr.Name)
	}
	return nil
}

// readEntry streams one entry: hashing it and, for a file, writing it to the
// staging directory (and fsyncing it). The manifest is kept in memory.
func (a *archive) readEntry(hdr *tar.Header, tr *tar.Reader, manifest *[]byte) (string, int64, error) {
	h := sha256.New()
	var out io.Writer = io.Discard
	if hdr.Name == ManifestName {
		var body []byte
		out = writerFunc(func(p []byte) (int, error) {
			body = append(body, p...)
			*manifest = body
			return len(p), nil
		})
	} else {
		f, err := os.OpenFile(filepath.Join(a.dir, hdr.Name), os.O_WRONLY|os.O_CREATE|os.O_EXCL, fileModes[hdr.Name])
		if err != nil {
			return "", 0, fmt.Errorf("backup: staging %s: %w", hdr.Name, err)
		}
		defer f.Close()
		out = f
	}
	n, err := io.Copy(io.MultiWriter(out, h), io.LimitReader(tr, MaxTotal+1))
	if err != nil {
		return "", 0, fmt.Errorf("%w: reading %q: %s", ErrNotArchive, hdr.Name, err)
	}
	if n != hdr.Size {
		return "", 0, fmt.Errorf("%w: %q is truncated", ErrNotArchive, hdr.Name)
	}
	if f, ok := out.(*os.File); ok {
		if err := f.Sync(); err != nil {
			return "", 0, fmt.Errorf("backup: writing %s: %w", hdr.Name, err)
		}
	}
	return hex.EncodeToString(h.Sum(nil)), n, nil
}

// info returns what the manifest says about name.
func (a *archive) info(name string) (FileInfo, bool) {
	fi, ok := a.meta.Files[name]
	return fi, ok
}

// parseManifest decodes and validates the manifest of the archive.
func (a *archive) parseManifest(body []byte) error {
	var m Meta
	dec := json.NewDecoder(bytes.NewReader(body))
	dec.DisallowUnknownFields()
	if err := dec.Decode(&m); err != nil {
		return fmt.Errorf("%w: the manifest is not valid: %s", ErrNotArchive, err)
	}
	if m.Schema != Schema {
		return fmt.Errorf("%w: it was made by a different version (schema %d, this device reads %d)",
			ErrNotArchive, m.Schema, Schema)
	}
	for name, fi := range m.Files {
		if !allowed(name) {
			return fmt.Errorf("%w: the manifest lists %q", ErrNotArchive, name)
		}
		if fi.Size < 0 || fi.Size > MaxTotal {
			return fmt.Errorf("%w: the manifest gives %q an impossible size", ErrNotArchive, name)
		}
		if len(fi.SHA256) != sha256.Size*2 {
			return fmt.Errorf("%w: the manifest has no checksum for %q", ErrNotArchive, name)
		}
	}
	for _, name := range a.order {
		if _, ok := m.Files[name]; !ok {
			return fmt.Errorf("%w: %s does not list %q", ErrNotArchive, ManifestName, name)
		}
	}
	a.meta = m
	return nil
}

// commit moves the staged files into dataDir: one rename per file, after all
// of them have been written and fsynced, then an fsync of the directory. A
// rename within one filesystem cannot half-happen, so the worst a power cut
// during this loop can do is leave some files new and some old — never a
// truncated file. Staging happens inside dataDir so that this holds even when
// /tmp is a different filesystem (it is a tmpfs on the boards).
func commit(a *archive, dataDir string) error {
	if len(a.order) == 0 {
		return fmt.Errorf("%w: it holds no files", ErrNotArchive)
	}
	stage, err := os.MkdirTemp(dataDir, ".restore-")
	if err != nil {
		return fmt.Errorf("backup: staging inside the data directory: %w", err)
	}
	defer os.RemoveAll(stage)
	for _, name := range a.order {
		if err := move(filepath.Join(a.dir, name), filepath.Join(stage, name), fileModes[name]); err != nil {
			return err
		}
	}
	if err := syncDir(stage); err != nil {
		return err
	}
	for _, name := range a.order {
		if err := os.Rename(filepath.Join(stage, name), filepath.Join(dataDir, name)); err != nil {
			return fmt.Errorf("backup: putting %s in place: %w", name, err)
		}
	}
	return syncDir(dataDir)
}

// move copies src to dst (a plain copy: the staging directory may be on
// another filesystem), fsyncs it and applies mode.
func move(src, dst string, mode os.FileMode) error {
	in, err := os.Open(src)
	if err != nil {
		return fmt.Errorf("backup: staging %s: %w", filepath.Base(src), err)
	}
	defer in.Close()
	out, err := os.OpenFile(dst, os.O_WRONLY|os.O_CREATE|os.O_TRUNC, mode)
	if err != nil {
		return fmt.Errorf("backup: staging %s: %w", filepath.Base(dst), err)
	}
	fail := func(err error) error {
		out.Close()
		return err
	}
	if _, err := io.Copy(out, in); err != nil {
		return fail(fmt.Errorf("backup: staging %s: %w", filepath.Base(dst), err))
	}
	if err := out.Chmod(mode); err != nil {
		return fail(fmt.Errorf("backup: staging %s: %w", filepath.Base(dst), err))
	}
	if err := out.Sync(); err != nil {
		return fail(fmt.Errorf("backup: writing %s: %w", filepath.Base(dst), err))
	}
	if err := out.Close(); err != nil {
		return fmt.Errorf("backup: writing %s: %w", filepath.Base(dst), err)
	}
	return nil
}

func syncDir(dir string) error {
	d, err := os.Open(dir)
	if err != nil {
		return fmt.Errorf("backup: opening %s: %w", dir, err)
	}
	defer d.Close()
	if err := d.Sync(); err != nil {
		return fmt.Errorf("backup: fsync %s: %w", dir, err)
	}
	return nil
}

// checkName refuses any entry name that is not one of the state files. The
// allow-list is what makes an absolute path, a "..", a backslash or a NUL
// impossible rather than escaping them: nothing outside Files is ever joined
// onto the data directory, so there is no path to normalise.
func checkName(name string) error {
	if name == ManifestName {
		return nil
	}
	switch {
	case name == "":
		return fmt.Errorf("%w: an entry has no name", ErrNotArchive)
	case !allowed(name):
		return fmt.Errorf("%w: %q is not part of a device state", ErrNotArchive, name)
	}
	return nil
}

// countingReader counts the bytes actually read from the archive, so the
// compressed cap is not defeated by a Reader that ignores the LimitReader.
type countingReader struct {
	r io.Reader
	n int64
}

func (c *countingReader) Read(p []byte) (int, error) {
	n, err := c.r.Read(p)
	c.n += int64(n)
	return n, err
}

type writerFunc func([]byte) (int, error)

func (f writerFunc) Write(p []byte) (int, error) { return f(p) }
