// Command wat-manifest builds the release manifest consumed by the
// self-update in internal/update (docs/ARCHITECTURE.md section 11, decision
// D17).
//
// It reads the three cross-built binaries from -dist, hashes them and writes
// manifest.json with schema, version, release time, notes URL, an optional
// minimum-source version, one entry per runtime arch and, when -images points
// at them, one entry per board image (M10: beagle.img, cubie.img, rpi1.img,
// rpi2.img). The output is
// deterministic (fixed key order, indent 2) so re-running it on the same
// inputs produces byte-identical bytes and a signature stays valid.
//
// CI runs it on every tag; the owner then signs the result offline with
// tools/wat-sign.
package main

import (
	"bytes"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"flag"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"strconv"
	"time"
)

// schema is the manifest format version understood by internal/update.
const schema = 1

// assetFor maps a runtime.GOARCH/GOARM combination to the released file and
// the key used in the manifest. Order here is the JSON order.
type asset struct {
	key  string // manifest key, e.g. "linux-armv7"
	file string // file name inside -dist
}

var assets = []asset{
	{"linux-armv6", "wa-transcriber-linux-armv6"},
	{"linux-armv7", "wa-transcriber-linux-armv7"},
	{"linux-arm64", "wa-transcriber-linux-arm64"},
	{"linux-amd64", "wa-transcriber-linux-amd64"},
}

// manifest is the on-wire JSON. Field order is fixed by the struct so the
// encoder output is stable; json.Marshal never reorders struct fields.
type manifest struct {
	Schema     int              `json:"schema"`
	Version    string           `json:"version"`
	ReleasedAt string           `json:"released_at"`
	NotesURL   string           `json:"notes_url"`
	MinFrom    string           `json:"min_from"`
	Assets     map[string]entry `json:"assets"`
	// Images is written last and omitted when there is none, so a release
	// without board images produces exactly the bytes an older wat-manifest
	// did (a signature stays valid across a rebuild).
	Images map[string]imageEntry `json:"images,omitempty"`
}

// imageEntry is one board image: the file, its hash and size, and the
// partition-table version a device needs to install it (2 = p1 boot, p2 root
// A, p3 root B, p4 data).
type imageEntry struct {
	File   string `json:"file"`
	SHA256 string `json:"sha256"`
	Size   int64  `json:"size"`
	Layout int    `json:"layout,omitempty"`
}

// images maps a board key to the file name inside -images. The keys are the
// boards, not architectures: the same armv7 binary runs on BeagleBone,
// Cubieboard and Pi 2.
var images = []struct{ key, file string }{
	{"beagle", "beagle.img"},
	{"cubie", "cubie.img"},
	{"rpi1", "rpi1.img"},
	{"rpi2", "rpi2.img"},
}

// layoutVersion is the partition table the images in this release have.
const layoutVersion = 2

type entry struct {
	File   string `json:"file"`
	SHA256 string `json:"sha256"`
	Size   int64  `json:"size"`
}

func main() {
	dist := flag.String("dist", "dist", "directory holding the cross-built binaries")
	version := flag.String("version", "", "release version, e.g. v0.2.0 (required)")
	out := flag.String("out", "", "output manifest path (default <dist>/manifest.json)")
	minFrom := flag.String("min-from", "", "oldest version allowed to update (empty = no constraint)")
	notesURL := flag.String("notes-url", "", "release notes URL (default the GitHub tag page for -version)")
	imagesDir := flag.String("images", "", "directory holding board images (beagle.img, cubie.img, rpi1.img, rpi2.img; empty: no images in the manifest, as in releases before M10)")
	flag.Usage = func() {
		fmt.Fprintf(flag.CommandLine.Output(), "usage: wat-manifest -version v0.2.0 [-dist dist] [-images dir] [-out dist/manifest.json]\n\n")
		flag.PrintDefaults()
	}
	flag.Parse()

	outPath := *out
	if outPath == "" {
		outPath = filepath.Join(*dist, "manifest.json")
	}

	if err := run(*dist, *version, *minFrom, *notesURL, *imagesDir, outPath); err != nil {
		fmt.Fprintf(os.Stderr, "wat-manifest: %v\n", err)
		os.Exit(1)
	}
}

func run(dist, version, minFrom, notesURL, imagesDir, outPath string) error {
	if version == "" {
		return errors.New("-version is required")
	}
	if notesURL == "" {
		notesURL = "https://github.com/emdioh/wa-transcriber/releases/tag/" + version
	}

	m := manifest{
		Schema:     schema,
		Version:    version,
		ReleasedAt: releaseTime(),
		NotesURL:   notesURL,
		MinFrom:    minFrom,
		Assets:     make(map[string]entry, len(assets)),
	}

	for _, a := range assets {
		path := filepath.Join(dist, a.file)
		sum, size, err := hashFile(path)
		if err != nil {
			// A missing binary is a hard error naming the exact file: CI must
			// never publish a manifest that points at an asset it did not build.
			if errors.Is(err, os.ErrNotExist) {
				return fmt.Errorf("missing binary %s", path)
			}
			return err
		}
		m.Assets[a.key] = entry{File: a.file, SHA256: sum, Size: size}
	}

	if imagesDir != "" {
		m.Images = make(map[string]imageEntry, len(images))
		for _, img := range images {
			path := filepath.Join(imagesDir, img.file)
			sum, size, err := hashFile(path)
			if err != nil {
				if errors.Is(err, os.ErrNotExist) {
					// An older release, or a build that did not make the
					// board images: not an error, just no images block.
					continue
				}
				return err
			}
			m.Images[img.key] = imageEntry{File: img.file, SHA256: sum, Size: size, Layout: layoutVersion}
		}
		if len(m.Images) == 0 {
			m.Images = nil
		}
	}

	var buf bytes.Buffer
	enc := json.NewEncoder(&buf)
	enc.SetEscapeHTML(false)
	enc.SetIndent("", "  ")
	if err := enc.Encode(m); err != nil {
		return fmt.Errorf("encode manifest: %w", err)
	}

	if err := os.WriteFile(outPath, buf.Bytes(), 0o644); err != nil {
		return fmt.Errorf("write manifest: %w", err)
	}
	return nil
}

// releaseTime is the manifest timestamp: SOURCE_DATE_EPOCH when set (so a
// rebuild is reproducible), otherwise the current UTC time.
func releaseTime() string {
	if v := os.Getenv("SOURCE_DATE_EPOCH"); v != "" {
		if secs, err := strconv.ParseInt(v, 10, 64); err == nil {
			return time.Unix(secs, 0).UTC().Format(time.RFC3339)
		}
	}
	return time.Now().UTC().Format(time.RFC3339)
}

// hashFile returns the hex sha256 and the byte size of path.
func hashFile(path string) (string, int64, error) {
	f, err := os.Open(path)
	if err != nil {
		return "", 0, err
	}
	defer f.Close()

	h := sha256.New()
	size, err := io.Copy(h, f)
	if err != nil {
		return "", 0, fmt.Errorf("read %s: %w", path, err)
	}
	return hex.EncodeToString(h.Sum(nil)), size, nil
}
