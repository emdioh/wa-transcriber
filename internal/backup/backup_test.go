package backup

import (
	"archive/tar"
	"bytes"
	"compress/gzip"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"io"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

var fixed = time.Date(2026, 11, 23, 12, 0, 0, 0, time.UTC)

// device makes a data directory that looks like a paired device.
func device(t *testing.T) string {
	t.Helper()
	dir := t.TempDir()
	for name, body := range map[string]string{
		"config.json":    `{"web":{"password_hash":"$2a$10$x"},"transcription":{"api_key":"sk-secret"}}` + "\n",
		"session.db":     "sqlite-ish bytes",
		"session.db-wal": "wal",
		"wifi.json":      `{"networks":[{"ssid":"home","psk":"hunter2"}]}`,
	} {
		if err := os.WriteFile(filepath.Join(dir, name), []byte(body), 0o600); err != nil {
			t.Fatal(err)
		}
	}
	return dir
}

func write(t *testing.T, dir string, meta Meta) []byte {
	t.Helper()
	var buf bytes.Buffer
	if err := Write(&buf, dir, meta); err != nil {
		t.Fatal(err)
	}
	return buf.Bytes()
}

// sandbox is a restore target: a directory that already holds a device state.
func sandbox(t *testing.T) string {
	t.Helper()
	dir := t.TempDir()
	if err := os.WriteFile(filepath.Join(dir, "config.json"), []byte(`{"old":true}`), 0o600); err != nil {
		t.Fatal(err)
	}
	return dir
}

// snapshot records every name and content of dir, so a test can prove that a
// failed restore wrote nothing at all.
func snapshot(t *testing.T, dir string) map[string]string {
	t.Helper()
	entries, err := os.ReadDir(dir)
	if err != nil {
		t.Fatal(err)
	}
	out := map[string]string{}
	for _, e := range entries {
		b, err := os.ReadFile(filepath.Join(dir, e.Name()))
		if err != nil {
			// A directory left behind is a change too.
			out[e.Name()] = "<dir>"
			continue
		}
		out[e.Name()] = string(b)
	}
	return out
}

func same(a, b map[string]string) bool {
	if len(a) != len(b) {
		return false
	}
	for k, v := range a {
		if b[k] != v {
			return false
		}
	}
	return true
}

func entries(t *testing.T, b []byte) []*tar.Header {
	t.Helper()
	gz, err := gzip.NewReader(bytes.NewReader(b))
	if err != nil {
		t.Fatal(err)
	}
	tr := tar.NewReader(gz)
	var out []*tar.Header
	for {
		hdr, err := tr.Next()
		if errors.Is(err, io.EOF) {
			return out
		}
		if err != nil {
			t.Fatal(err)
		}
		out = append(out, hdr)
	}
}

func TestRoundTrip(t *testing.T) {
	src := device(t)
	archive := write(t, src, Meta{Version: "v0.3.0", CreatedAt: fixed})

	dst := t.TempDir()
	meta, err := Restore(bytes.NewReader(archive), dst)
	if err != nil {
		t.Fatal(err)
	}
	if meta.Schema != Schema || meta.Version != "v0.3.0" || !meta.CreatedAt.Equal(fixed) {
		t.Fatalf("meta = %+v", meta)
	}
	want := map[string]string{
		"config.json":    `{"web":{"password_hash":"$2a$10$x"},"transcription":{"api_key":"sk-secret"}}` + "\n",
		"session.db":     "sqlite-ish bytes",
		"session.db-wal": "wal",
		"wifi.json":      `{"networks":[{"ssid":"home","psk":"hunter2"}]}`,
	}
	if got := snapshot(t, dst); !same(got, want) {
		t.Fatalf("restored %v, want %v", got, want)
	}
	if _, err := os.Stat(filepath.Join(dst, "session.db-shm")); !errors.Is(err, os.ErrNotExist) {
		t.Fatal("a file that was not in the archive was created")
	}
	for name, mode := range map[string]os.FileMode{"config.json": 0o600, "session.db": 0o644, "wifi.json": 0o600} {
		fi, err := os.Stat(filepath.Join(dst, name))
		if err != nil {
			t.Fatal(err)
		}
		if fi.Mode().Perm() != mode {
			t.Fatalf("%s is %v, want %v", name, fi.Mode().Perm(), mode)
		}
	}
	// The staging directories are gone, inside and outside the target.
	entries, err := os.ReadDir(dst)
	if err != nil {
		t.Fatal(err)
	}
	for _, e := range entries {
		if strings.HasPrefix(e.Name(), ".") {
			t.Fatalf("left %q behind", e.Name())
		}
	}
}

func TestManifestComesFirst(t *testing.T) {
	archive := write(t, device(t), Meta{Version: "test", CreatedAt: fixed})
	hdr := entries(t, archive)
	if len(hdr) != 5 || hdr[0].Name != ManifestName {
		var names []string
		for _, h := range hdr {
			names = append(names, h.Name)
		}
		t.Fatalf("entries %v, want %s first", names, ManifestName)
	}
	want := append([]string{"config.json"}, Files...)
	_ = want
	var got []string
	for _, h := range hdr {
		got = append(got, h.Name)
	}
	if strings.Join(got, ",") != "manifest.json,config.json,session.db,session.db-wal,wifi.json" {
		t.Fatalf("order %v", got)
	}
	for _, h := range hdr {
		if !h.ModTime.Equal(fixed) {
			t.Fatalf("%s has mtime %v, want the archive's CreatedAt", h.Name, h.ModTime)
		}
		if h.Uid != 0 || h.Gid != 0 || h.Uname != "" || h.Gname != "" {
			t.Fatalf("%s carries host ownership %d/%d %q/%q", h.Name, h.Uid, h.Gid, h.Uname, h.Gname)
		}
	}
}

func TestDeterministic(t *testing.T) {
	dir := device(t)
	a := write(t, dir, Meta{Version: "v1", CreatedAt: fixed})
	b := write(t, dir, Meta{Version: "v1", CreatedAt: fixed})
	if !bytes.Equal(a, b) {
		t.Fatal("two writes of the same input differ")
	}
	c := write(t, dir, Meta{Version: "v1", CreatedAt: fixed.Add(time.Second)})
	if bytes.Equal(a, c) {
		t.Fatal("a different CreatedAt gave the same bytes")
	}
}

func TestDeviceWithoutWiFi(t *testing.T) {
	dir := t.TempDir()
	if err := os.WriteFile(filepath.Join(dir, "config.json"), []byte("{}"), 0o600); err != nil {
		t.Fatal(err)
	}
	archive := write(t, dir, Meta{CreatedAt: fixed})
	meta, err := Verify(bytes.NewReader(archive))
	if err != nil {
		t.Fatal(err)
	}
	if len(meta.Files) != 1 {
		t.Fatalf("files = %v", meta.Files)
	}
	if _, err := Restore(bytes.NewReader(archive), t.TempDir()); err != nil {
		t.Fatal(err)
	}
}

// rebuild produces an archive with a fresh manifest: mutate changes the file
// bodies, and the checksums are recomputed from what is actually there.
func rebuild(t *testing.T, archive []byte, mutate func(name string, body []byte) (string, []byte), manifest func(m map[string]any)) []byte {
	t.Helper()
	gz, err := gzip.NewReader(bytes.NewReader(archive))
	if err != nil {
		t.Fatal(err)
	}
	tr := tar.NewReader(gz)
	type file struct {
		name string
		body []byte
	}
	var files []file
	var man map[string]any
	for {
		hdr, err := tr.Next()
		if errors.Is(err, io.EOF) {
			break
		}
		if err != nil {
			t.Fatal(err)
		}
		body, _ := io.ReadAll(tr)
		if hdr.Name == ManifestName {
			if err := json.Unmarshal(body, &man); err != nil {
				t.Fatal(err)
			}
			continue
		}
		name, body := mutate(hdr.Name, body)
		files = append(files, file{name, body})
	}
	sums := map[string]any{}
	for _, f := range files {
		sum := sha256.Sum256(f.body)
		sums[f.name] = map[string]any{"sha256": hex.EncodeToString(sum[:]), "size": len(f.body)}
	}
	man["files"] = sums
	if manifest != nil {
		manifest(man)
	}
	manBody, _ := json.Marshal(man)
	var buf bytes.Buffer
	zw := gzip.NewWriter(&buf)
	tw := tar.NewWriter(zw)
	if err := tw.WriteHeader(&tar.Header{Name: ManifestName, Mode: 0o644, Size: int64(len(manBody)), Typeflag: tar.TypeReg}); err != nil {
		t.Fatal(err)
	}
	tw.Write(manBody)
	for _, f := range files {
		if err := tw.WriteHeader(&tar.Header{Name: f.name, Mode: 0o644, Size: int64(len(f.body)), Typeflag: tar.TypeReg}); err != nil {
			t.Fatal(err)
		}
		tw.Write(f.body)
	}
	tw.Close()
	zw.Close()
	return buf.Bytes()
}

func TestTamperedFileIsRefused(t *testing.T) {
	// Two archives that swap a byte in a file body without touching the
	// manifest: the checksums no longer match.
	good := write(t, device(t), Meta{CreatedAt: fixed})
	bad := rebuild(t, good, func(name string, body []byte) (string, []byte) {
		if name == "session.db" {
			body[0] ^= 0xff
		}
		return name, body
	}, func(m map[string]any) {
		// Restore the original checksums: the body is what changed.
		files := m["files"].(map[string]any)
		orig := files["session.db"].(map[string]any)
		sum := sha256.Sum256([]byte("sqlite-ish bytes"))
		orig["sha256"] = hex.EncodeToString(sum[:])
		files["session.db"] = orig
		m["files"] = files
	})
	dst := sandbox(t)
	before := snapshot(t, dst)
	if _, err := Restore(bytes.NewReader(bad), dst); !errors.Is(err, ErrNotArchive) {
		t.Fatalf("err = %v, want ErrNotArchive", err)
	}
	if !same(before, snapshot(t, dst)) {
		t.Fatalf("the data directory changed: %v -> %v", before, snapshot(t, dst))
	}
}

func TestManifestChecksumMismatch(t *testing.T) {
	good := write(t, device(t), Meta{CreatedAt: fixed})
	bad := rebuild(t, good, func(name string, body []byte) (string, []byte) { return name, body },
		func(m map[string]any) {
			files := m["files"].(map[string]any)
			fi := files["config.json"].(map[string]any)
			fi["sha256"] = strings.Repeat("0", 64)
			files["config.json"] = fi
		})
	if _, err := Restore(bytes.NewReader(bad), sandbox(t)); err == nil ||
		!strings.Contains(err.Error(), "checksum") {
		t.Fatalf("err = %v", err)
	}
}

func TestMissingManifest(t *testing.T) {
	var buf bytes.Buffer
	zw := gzip.NewWriter(&buf)
	tw := tar.NewWriter(zw)
	tw.WriteHeader(&tar.Header{Name: "session.db", Mode: 0o644, Size: 2, Typeflag: tar.TypeReg})
	tw.Write([]byte("db"))
	tw.Close()
	zw.Close()
	dst := sandbox(t)
	before := snapshot(t, dst)
	if _, err := Restore(bytes.NewReader(buf.Bytes()), dst); !errors.Is(err, ErrNotArchive) {
		t.Fatalf("err = %v", err)
	}
	if !same(before, snapshot(t, dst)) {
		t.Fatal("the data directory changed")
	}
}

// A manifest at the end, after the files it describes, is not an archive this
// package writes: refuse it rather than check files against nothing.
func TestLateManifestIsRefused(t *testing.T) {
	var buf bytes.Buffer
	zw := gzip.NewWriter(&buf)
	tw := tar.NewWriter(zw)
	body := []byte("db")
	sum := sha256.Sum256(body)
	man, _ := json.Marshal(Meta{Schema: Schema, CreatedAt: fixed,
		Files: map[string]FileInfo{"session.db": {SHA256: hex.EncodeToString(sum[:]), Size: int64(len(body))}}})
	tw.WriteHeader(&tar.Header{Name: "session.db", Mode: 0o644, Size: int64(len(body)), Typeflag: tar.TypeReg})
	tw.Write(body)
	tw.WriteHeader(&tar.Header{Name: ManifestName, Mode: 0o644, Size: int64(len(man)), Typeflag: tar.TypeReg})
	tw.Write(man)
	tw.Close()
	zw.Close()
	dst := sandbox(t)
	before := snapshot(t, dst)
	if _, err := Restore(bytes.NewReader(buf.Bytes()), dst); !strings.Contains(err.Error(), "not the first entry") {
		t.Fatalf("err = %v", err)
	}
	if !same(before, snapshot(t, dst)) {
		t.Fatal("the data directory changed")
	}
}

func TestUnknownSchema(t *testing.T) {
	good := write(t, device(t), Meta{CreatedAt: fixed})
	bad := rebuild(t, good, func(name string, body []byte) (string, []byte) { return name, body },
		func(m map[string]any) { m["schema"] = Schema + 1 })
	if _, err := Verify(bytes.NewReader(bad)); !strings.Contains(err.Error(), "schema") {
		t.Fatalf("err = %v", err)
	}
	if _, err := Restore(bytes.NewReader(bad), sandbox(t)); !strings.Contains(err.Error(), "schema") {
		t.Fatalf("err = %v", err)
	}
}

func TestPathTraversalIsRefused(t *testing.T) {
	for _, name := range []string{"../../etc/passwd", "/etc/passwd", `..\..\etc\passwd`, "sub/config.json"} {
		var buf bytes.Buffer
		zw := gzip.NewWriter(&buf)
		tw := tar.NewWriter(zw)
		man, _ := json.Marshal(Meta{Schema: Schema, CreatedAt: fixed, Files: map[string]FileInfo{}})
		tw.WriteHeader(&tar.Header{Name: ManifestName, Mode: 0o644, Size: int64(len(man)), Typeflag: tar.TypeReg})
		tw.Write(man)
		tw.WriteHeader(&tar.Header{Name: name, Mode: 0o644, Size: 1, Typeflag: tar.TypeReg})
		tw.Write([]byte("x"))
		tw.Close()
		zw.Close()
		if _, err := Restore(bytes.NewReader(buf.Bytes()), sandbox(t)); !errors.Is(err, ErrNotArchive) {
			t.Fatalf("%q: err = %v, want ErrNotArchive", name, err)
		}
	}
	if _, err := os.Stat("/tmp/passwd"); err == nil {
		t.Fatal("a traversal wrote out of the data directory")
	}
}

func TestFileOutsideTheAllowList(t *testing.T) {
	var buf bytes.Buffer
	zw := gzip.NewWriter(&buf)
	tw := tar.NewWriter(zw)
	man, _ := json.Marshal(Meta{Schema: Schema, CreatedAt: fixed,
		Files: map[string]FileInfo{"updates/boot.json": {SHA256: strings.Repeat("a", 64), Size: 1}}})
	tw.WriteHeader(&tar.Header{Name: ManifestName, Mode: 0o644, Size: int64(len(man)), Typeflag: tar.TypeReg})
	tw.Write(man)
	tw.WriteHeader(&tar.Header{Name: "updates/boot.json", Mode: 0o644, Size: 1, Typeflag: tar.TypeReg})
	tw.Write([]byte("x"))
	tw.Close()
	zw.Close()
	if _, err := Restore(bytes.NewReader(buf.Bytes()), sandbox(t)); !errors.Is(err, ErrNotArchive) {
		t.Fatalf("err = %v", err)
	}
}

func TestSymlinkEntryIsRefused(t *testing.T) {
	var buf bytes.Buffer
	zw := gzip.NewWriter(&buf)
	tw := tar.NewWriter(zw)
	man, _ := json.Marshal(Meta{Schema: Schema, CreatedAt: fixed, Files: map[string]FileInfo{}})
	tw.WriteHeader(&tar.Header{Name: ManifestName, Mode: 0o644, Size: int64(len(man)), Typeflag: tar.TypeReg})
	tw.Write(man)
	tw.WriteHeader(&tar.Header{Name: "session.db", Linkname: "/etc/shadow", Typeflag: tar.TypeSymlink})
	tw.Close()
	zw.Close()
	if _, err := Restore(bytes.NewReader(buf.Bytes()), sandbox(t)); !strings.Contains(err.Error(), "regular file") {
		t.Fatalf("err = %v", err)
	}
}

// An archive that unpacks to a bomb is cut off by the total cap, long before
// it could fill the card.
func TestOversizedUncompressedIsRefused(t *testing.T) {
	var buf bytes.Buffer
	zw := gzip.NewWriter(&buf)
	tw := tar.NewWriter(zw)
	huge := int64(MaxTotal) + 1
	tw.WriteHeader(&tar.Header{Name: "session.db", Mode: 0o644, Size: huge, Typeflag: tar.TypeReg})
	if err := tw.Close(); err != nil {
		// tar refuses to close with a short entry: the point of the test is
		// what the reader does with the entry it declared.
		t.Logf("tar close: %v", err)
	}
	zw.Close()
	if _, err := Verify(bytes.NewReader(buf.Bytes())); !errors.Is(err, ErrNotArchive) {
		t.Fatalf("err = %v, want ErrNotArchive", err)
	}
}

func TestOversizedCompressedIsRefused(t *testing.T) {
	// A gzip stream of incompressible bytes, larger than the compressed cap.
	// Written straight into the file, not through bytes.Buffer: growing a
	// 64 MB slice by appending is what makes this test slow.
	f, err := os.CreateTemp(t.TempDir(), "big-*.gz")
	if err != nil {
		t.Fatal(err)
	}
	defer f.Close()
	zw, _ := gzip.NewWriterLevel(f, gzip.BestSpeed)
	for written := 0; written <= MaxArchive; written += 1 << 16 {
		if _, err := zw.Write(zeroChunk); err != nil {
			t.Fatal(err)
		}
	}
	zw.Close()
	f.Seek(0, io.SeekStart)
	if _, err := Verify(f); !errors.Is(err, ErrNotArchive) {
		t.Fatalf("err = %v, want ErrNotArchive", err)
	}
}

var zeroChunk = bytes.Repeat([]byte{0x00}, 1<<16)

func TestTruncatedArchive(t *testing.T) {
	full := write(t, device(t), Meta{CreatedAt: fixed})
	// Some cuts land exactly on a block boundary of the gzip stream and
	// still decode; whatever the outcome, the directory must not change.
	for _, cut := range []int{0, 5, len(full) / 2, len(full) - 3} {
		dst := sandbox(t)
		before := snapshot(t, dst)
		if _, err := Restore(bytes.NewReader(full[:cut]), dst); err != nil {
			if !same(before, snapshot(t, dst)) {
				t.Fatalf("cut at %d: the data directory changed", cut)
			}
		}
	}
	// Cut inside the tar, with a valid gzip wrapper: the last file's body is
	// short, which the size check catches.
	var raw bytes.Buffer
	gz, _ := gzip.NewReader(bytes.NewReader(full))
	io.Copy(&raw, gz)
	short := raw.Bytes()[:len(raw.Bytes())-20]
	var out bytes.Buffer
	zw := gzip.NewWriter(&out)
	zw.Write(short)
	zw.Close()
	dst := sandbox(t)
	before := snapshot(t, dst)
	if _, err := Restore(bytes.NewReader(out.Bytes()), dst); err == nil {
		t.Fatal("a truncated tar was accepted")
	}
	if !same(before, snapshot(t, dst)) {
		t.Fatal("the data directory changed")
	}
}

// The test the whole design exists for: a restore that fails at the last
// moment leaves the device exactly as it was.
func TestFailedRestoreLeavesTheDataDirectoryUntouched(t *testing.T) {
	good := write(t, device(t), Meta{CreatedAt: fixed})
	// The manifest lists config.json, but its entry comes last; a tampered
	// byte in it is found after the earlier files were staged.
	bad := rebuild(t, good, func(name string, body []byte) (string, []byte) {
		if name == "wifi.json" {
			body[5] ^= 0xff
		}
		return name, body
	}, func(m map[string]any) {
		fi := m["files"].(map[string]any)["wifi.json"].(map[string]any)
		fi["sha256"] = strings.Repeat("b", 64)
	})
	dst := sandbox(t)
	before := snapshot(t, dst)
	_, err := Restore(bytes.NewReader(bad), dst)
	if !errors.Is(err, ErrNotArchive) {
		t.Fatalf("err = %v", err)
	}
	after := snapshot(t, dst)
	if !same(before, after) {
		t.Fatalf("the data directory changed:\nbefore %v\nafter  %v", before, after)
	}
}

func TestRestoreIntoMissingDirectoryFails(t *testing.T) {
	good := write(t, device(t), Meta{CreatedAt: fixed})
	dir := filepath.Join(t.TempDir(), "nope")
	if _, err := Restore(bytes.NewReader(good), dir); err == nil {
		t.Fatal("a restore into a missing directory succeeded")
	}
	if _, err := os.Stat(dir); !errors.Is(err, os.ErrNotExist) {
		t.Fatal("the directory was created")
	}
}

func TestVerifyWritesNothing(t *testing.T) {
	good := write(t, device(t), Meta{Version: "v1", CreatedAt: fixed})
	meta, err := Verify(bytes.NewReader(good))
	if err != nil {
		t.Fatal(err)
	}
	if names := meta.Names(); strings.Join(names, ",") != "config.json,session.db,session.db-wal,wifi.json" {
		t.Fatalf("names %v", names)
	}
	// rebuild recomputes the checksums, so flip the body of a copy of the
	// archive without touching the manifest it carries.
	bad := rebuild(t, good, func(name string, body []byte) (string, []byte) {
		if name == "config.json" {
			body[0] ^= 0xff
		}
		return name, body
	}, func(m map[string]any) {
		fi := m["files"].(map[string]any)["config.json"].(map[string]any)
		fi["sha256"] = "00" + strings.Repeat("0", 62)
	})
	if _, err := Verify(bytes.NewReader(bad)); err == nil {
		t.Fatal("verify accepted a tampered archive")
	}
}

func TestWriteSkipsSymlinks(t *testing.T) {
	dir := t.TempDir()
	if err := os.WriteFile(filepath.Join(dir, "config.json"), []byte("{}"), 0o600); err != nil {
		t.Fatal(err)
	}
	secret := filepath.Join(t.TempDir(), "secret")
	os.WriteFile(secret, []byte("not a session"), 0o600)
	if err := os.Symlink(secret, filepath.Join(dir, "session.db")); err != nil {
		t.Skip("no symlinks here:", err)
	}
	archive := write(t, dir, Meta{CreatedAt: fixed})
	meta, err := Verify(bytes.NewReader(archive))
	if err != nil {
		t.Fatal(err)
	}
	if _, ok := meta.Files["session.db"]; ok {
		t.Fatal("a symlink was archived")
	}
}

func TestCompressedCapConstant(t *testing.T) {
	if MaxArchive != 64<<20 || MaxTotal != 128<<20 {
		t.Fatalf("caps changed: %d %d", MaxArchive, MaxTotal)
	}
}
