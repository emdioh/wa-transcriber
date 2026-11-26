package main

import (
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"os"
	"path/filepath"
	"testing"
)

// writeFakeAssets creates the three files wat-manifest expects in dist and
// returns the sha256 and size of each, keyed by manifest key.
type want struct {
	sha  string
	size int64
}

func writeFakeAssets(t *testing.T, dist string) map[string]want {
	t.Helper()
	contents := map[string][]byte{
		"linux-armv6": []byte("armv6"),
		"linux-armv7": []byte("armv7-binary"),
		"linux-arm64": []byte("arm64-binary-longer"),
		"linux-amd64": []byte("amd64"),
	}
	files := map[string]string{
		"linux-armv6": "wa-transcriber-linux-armv6",
		"linux-armv7": "wa-transcriber-linux-armv7",
		"linux-arm64": "wa-transcriber-linux-arm64",
		"linux-amd64": "wa-transcriber-linux-amd64",
	}
	out := make(map[string]want, len(contents))
	for key, body := range contents {
		if err := os.WriteFile(filepath.Join(dist, files[key]), body, 0o755); err != nil {
			t.Fatalf("write fake %s: %v", files[key], err)
		}
		sum := sha256.Sum256(body)
		out[key] = want{sha: hex.EncodeToString(sum[:]), size: int64(len(body))}
	}
	return out
}

func TestManifestFromDist(t *testing.T) {
	dist := t.TempDir()
	wantAssets := writeFakeAssets(t, dist)
	out := filepath.Join(dist, "manifest.json")

	if err := run(dist, "v0.2.0", "v0.1.0", "", "", out); err != nil {
		t.Fatalf("run: %v", err)
	}

	raw, err := os.ReadFile(out)
	if err != nil {
		t.Fatalf("read manifest: %v", err)
	}

	var m struct {
		Schema     int    `json:"schema"`
		Version    string `json:"version"`
		ReleasedAt string `json:"released_at"`
		NotesURL   string `json:"notes_url"`
		MinFrom    string `json:"min_from"`
		Assets     map[string]struct {
			File   string `json:"file"`
			SHA256 string `json:"sha256"`
			Size   int64  `json:"size"`
		} `json:"assets"`
	}
	if err := json.Unmarshal(raw, &m); err != nil {
		t.Fatalf("parse manifest: %v\n%s", err, raw)
	}

	if m.Schema != schema {
		t.Errorf("schema = %d, want %d", m.Schema, schema)
	}
	if m.Version != "v0.2.0" {
		t.Errorf("version = %q, want v0.2.0", m.Version)
	}
	if m.MinFrom != "v0.1.0" {
		t.Errorf("min_from = %q, want v0.1.0", m.MinFrom)
	}
	if m.NotesURL != "https://github.com/emdioh/wa-transcriber/releases/tag/v0.2.0" {
		t.Errorf("notes_url = %q", m.NotesURL)
	}
	if m.ReleasedAt == "" {
		t.Error("released_at is empty")
	}
	if len(m.Assets) != len(wantAssets) {
		t.Fatalf("assets = %d, want %d", len(m.Assets), len(wantAssets))
	}
	for key, w := range wantAssets {
		got, ok := m.Assets[key]
		if !ok {
			t.Errorf("missing asset %q", key)
			continue
		}
		if got.SHA256 != w.sha {
			t.Errorf("%s sha256 = %q, want %q", key, got.SHA256, w.sha)
		}
		if got.Size != w.size {
			t.Errorf("%s size = %d, want %d", key, got.Size, w.size)
		}
		if got.File != "wa-transcriber-"+key {
			t.Errorf("%s file = %q", key, got.File)
		}
	}
}

func TestManifestDeterministic(t *testing.T) {
	dist := t.TempDir()
	writeFakeAssets(t, dist)

	if err := os.Setenv("SOURCE_DATE_EPOCH", "1700000000"); err != nil {
		t.Fatal(err)
	}
	defer os.Unsetenv("SOURCE_DATE_EPOCH")

	first := filepath.Join(dist, "a.json")
	second := filepath.Join(dist, "b.json")
	if err := run(dist, "v0.2.0", "", "", "", first); err != nil {
		t.Fatalf("first run: %v", err)
	}
	if err := run(dist, "v0.2.0", "", "", "", second); err != nil {
		t.Fatalf("second run: %v", err)
	}

	a, _ := os.ReadFile(first)
	b, _ := os.ReadFile(second)
	if string(a) != string(b) {
		t.Errorf("output not deterministic:\n%s\n---\n%s", a, b)
	}

	var m struct {
		ReleasedAt string `json:"released_at"`
	}
	if err := json.Unmarshal(a, &m); err != nil {
		t.Fatal(err)
	}
	if m.ReleasedAt != "2023-11-14T22:13:20Z" {
		t.Errorf("released_at = %q, want SOURCE_DATE_EPOCH value", m.ReleasedAt)
	}
}

func TestManifestMissingBinary(t *testing.T) {
	dist := t.TempDir()
	writeFakeAssets(t, dist)
	if err := os.Remove(filepath.Join(dist, "wa-transcriber-linux-arm64")); err != nil {
		t.Fatal(err)
	}

	err := run(dist, "v0.2.0", "", "", "", filepath.Join(dist, "manifest.json"))
	if err == nil {
		t.Fatal("run succeeded with a missing binary, want error")
	}
	if want := "wa-transcriber-linux-arm64"; !contains(err.Error(), want) {
		t.Errorf("error %q does not name %q", err, want)
	}
	if _, statErr := os.Stat(filepath.Join(dist, "manifest.json")); statErr == nil {
		t.Error("manifest was written despite a missing binary")
	}
}

func TestManifestRequiresVersion(t *testing.T) {
	dist := t.TempDir()
	writeFakeAssets(t, dist)

	if err := run(dist, "", "", "", "", filepath.Join(dist, "manifest.json")); err == nil {
		t.Fatal("run succeeded without -version, want error")
	}
}

func contains(s, sub string) bool {
	for i := 0; i+len(sub) <= len(s); i++ {
		if s[i:i+len(sub)] == sub {
			return true
		}
	}
	return false
}

// readImages decodes the images block of a manifest, nil when absent.
func readImages(t *testing.T, path string) map[string]struct {
	File   string `json:"file"`
	SHA256 string `json:"sha256"`
	Size   int64  `json:"size"`
	Layout int    `json:"layout"`
} {
	t.Helper()
	raw, err := os.ReadFile(path)
	if err != nil {
		t.Fatalf("read manifest: %v", err)
	}
	var m struct {
		Images map[string]struct {
			File   string `json:"file"`
			SHA256 string `json:"sha256"`
			Size   int64  `json:"size"`
			Layout int    `json:"layout"`
		} `json:"images"`
	}
	if err := json.Unmarshal(raw, &m); err != nil {
		t.Fatalf("decode manifest: %v", err)
	}
	return m.Images
}

func TestManifestWithImages(t *testing.T) {
	dist := t.TempDir()
	writeFakeAssets(t, dist)
	imgDir := t.TempDir()
	bodies := map[string]string{
		"beagle.img": "beagle-rootfs",
		"cubie.img":  "cubie-rootfs-sun4i",
		"rpi1.img":   "rpi1-rootfs-armv6",
		"rpi2.img":   "rpi2-rootfs-longer",
	}
	for name, body := range bodies {
		if err := os.WriteFile(filepath.Join(imgDir, name), []byte(body), 0o644); err != nil {
			t.Fatal(err)
		}
	}
	out := filepath.Join(dist, "manifest.json")
	if err := run(dist, "v1.2.0", "", "", imgDir, out); err != nil {
		t.Fatalf("run: %v", err)
	}
	got := readImages(t, out)
	if len(got) != len(bodies) {
		t.Fatalf("images = %v, want beagle, cubie, rpi1 and rpi2", got)
	}
	for _, name := range []string{"beagle.img", "cubie.img", "rpi1.img", "rpi2.img"} {
		key := name[:len(name)-len(".img")]
		img, ok := got[key]
		if !ok {
			t.Fatalf("no %s in %v", key, got)
		}
		if img.File != name {
			t.Errorf("%s file = %q", key, img.File)
		}
		sum := sha256.Sum256([]byte(bodies[name]))
		if img.SHA256 != hex.EncodeToString(sum[:]) {
			t.Errorf("%s sha256 = %q", key, img.SHA256)
		}
		if img.Layout != 2 {
			t.Errorf("%s layout = %d, want 2", key, img.Layout)
		}
	}
	// The binaries are still there, next to the images.
	raw, _ := os.ReadFile(out)
	if !contains(string(raw), "linux-armv7") {
		t.Error("the assets block disappeared when images were added")
	}
}

func TestManifestWithoutImages(t *testing.T) {
	dist := t.TempDir()
	writeFakeAssets(t, dist)
	out := filepath.Join(dist, "manifest.json")

	// An empty -images is a release without board images (before M10).
	if err := run(dist, "v0.2.0", "", "", "", out); err != nil {
		t.Fatalf("run: %v", err)
	}
	if got := readImages(t, out); got != nil {
		t.Fatalf("images = %v, want the block to be absent", got)
	}
	raw, _ := os.ReadFile(out)
	if contains(string(raw), "\"images\"") {
		t.Fatalf("an empty images block was written:\n%s", raw)
	}

	// A directory that does not contain them is not an error either: a
	// release job that built no images still publishes a manifest.
	empty := t.TempDir()
	if err := run(dist, "v0.2.0", "", "", empty, out); err != nil {
		t.Fatalf("run with an empty image dir: %v", err)
	}
	if got := readImages(t, out); got != nil {
		t.Fatalf("images = %v, want the block to be absent", got)
	}

	// One board image alone is published as one entry.
	if err := os.WriteFile(filepath.Join(empty, "beagle.img"), []byte("only-beagle"), 0o644); err != nil {
		t.Fatal(err)
	}
	if err := run(dist, "v0.2.0", "", "", empty, out); err != nil {
		t.Fatalf("run: %v", err)
	}
	got := readImages(t, out)
	if len(got) != 1 || got["beagle"].File != "beagle.img" {
		t.Fatalf("images = %v, want beagle only", got)
	}
}
