package main

import (
	"context"
	"crypto/ed25519"
	"crypto/sha256"
	"encoding/base64"
	"encoding/hex"
	"encoding/json"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"sync/atomic"
	"syscall"
	"testing"
	"time"

	"github.com/emdioh/wa-transcriber/internal/imageupdate"
	"github.com/emdioh/wa-transcriber/internal/update"
)

// fakeBoard is the A/B layout as files: two "partitions", a boot file, a data
// directory and the sysfs entries that make the running one identifiable.
// It is the command's view of internal/imageupdate's device, wired exactly the
// way main wires it (imageRequest → imageLayout → Service).
type fakeBoard struct {
	t    *testing.T
	dir  string
	req  imageRequest
	root string // the partition standing in for /
}

func newFakeBoard(t *testing.T) *fakeBoard {
	t.Helper()
	dir := t.TempDir()
	b := &fakeBoard{t: t, dir: dir, root: "mmcblk0p2"}
	for _, n := range []string{"mmcblk0p2", "mmcblk0p3"} {
		if err := os.WriteFile(filepath.Join(dir, n), nil, 0o644); err != nil {
			t.Fatal(err)
		}
	}
	b.req = imageRequest{
		Dir:    filepath.Join(dir, "data"),
		Layout: filepath.Join(dir, "image.json"),
		// The partitions are found by their sysfs names, which is what a
		// board has; DevDir points the writes at this directory instead.
		SysBlock: filepath.Join(dir, "sys"),
		DevDir:   dir + "/",
	}
	if err := os.MkdirAll(b.req.Dir, 0o700); err != nil {
		t.Fatal(err)
	}
	b.req.Root = filepath.Join(dir, "mmcblk0p2")
	b.req.BootRW = noopBootRW
	settings := map[string]any{
		"board":     "beagle",
		"boot_file": filepath.Join(dir, "extlinux.conf"),
		"slots":     []string{"p2", "p3"},
	}
	body, _ := json.Marshal(settings)
	if err := os.WriteFile(b.req.Layout, body, 0o644); err != nil {
		t.Fatal(err)
	}
	b.writeBootFile("mmcblk0p2")
	b.writeSys()
	return b
}

// writeSys makes the sysfs entry of the running partition carry the device
// number of the file that stands in for it.
func (b *fakeBoard) writeSys() {
	if err := os.RemoveAll(b.req.SysBlock); err != nil {
		b.t.Fatal(err)
	}
	fi, err := os.Stat(filepath.Join(b.dir, b.root))
	if err != nil {
		b.t.Fatal(err)
	}
	dev, ok := statDev(fi)
	if !ok {
		b.t.Fatal("the running partition has no device number")
	}
	major := dev >> 8 & 0xfff
	minor := (dev>>12)<<8 | dev&0xff
	p := filepath.Join(b.req.SysBlock, b.root)
	if err := os.MkdirAll(p, 0o755); err != nil {
		b.t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(p, "dev"), []byte(strconv.FormatUint(major, 10)+":"+strconv.FormatUint(minor, 10)), 0o644); err != nil {
		b.t.Fatal(err)
	}
}

// switchTo makes the fake device run from the other partition, as a reboot
// into the new slot would.
func (b *fakeBoard) switchTo(partition string) {
	b.root = partition
	b.writeSys()
}

func (b *fakeBoard) bootFile() string {
	body, _ := os.ReadFile(filepath.Join(b.dir, "extlinux.conf"))
	return string(body)
}

func (b *fakeBoard) writeBootFile(partition string) {
	conf := "DEFAULT wat\nLABEL wat\n  APPEND root=/dev/" + partition +
		" ro rootwait rootfstype=ext4 console=ttyO0,115200n8\n"
	if err := os.WriteFile(filepath.Join(b.dir, "extlinux.conf"), []byte(conf), 0o644); err != nil {
		b.t.Fatal(err)
	}
}

// imageServer serves a signed manifest with an images block and the image.
type imageServer struct {
	*httptest.Server
	manifest []byte
	sig      []byte
	img      []byte
	gets     atomic.Int32
}

func newImageServer(t *testing.T, priv ed25519.PrivateKey, m *update.Manifest, img []byte) *imageServer {
	t.Helper()
	body, err := json.Marshal(m)
	if err != nil {
		t.Fatal(err)
	}
	s := &imageServer{manifest: body, sig: ed25519.Sign(priv, body), img: img}
	mux := http.NewServeMux()
	mux.HandleFunc("/latest/download/manifest.json", func(w http.ResponseWriter, r *http.Request) {
		_, _ = w.Write(s.manifest)
	})
	mux.HandleFunc("/latest/download/manifest.json.ed25519", func(w http.ResponseWriter, r *http.Request) {
		_, _ = w.Write([]byte(base64.StdEncoding.EncodeToString(s.sig) + "\n"))
	})
	mux.HandleFunc("/latest/download/beagle.img", func(w http.ResponseWriter, r *http.Request) {
		s.gets.Add(1)
		_, _ = w.Write(s.img)
	})
	s.Server = httptest.NewServer(mux)
	t.Cleanup(s.Close)
	return s
}

// imageManifest is a release with one board image. The asset block is a
// placeholder: this test never installs the binary, and the manifest parser
// only needs it to be well formed.
func imageManifest(t *testing.T, version string, img []byte) *update.Manifest {
	t.Helper()
	sum := sha256.Sum256(img)
	key := ownAssetKey()
	if key == "" {
		key = "linux-armv7"
	}
	return &update.Manifest{
		Schema:     1,
		Version:    version,
		ReleasedAt: updateTestNow,
		Assets: map[string]update.Asset{
			key: {File: "wa-transcriber-" + version, SHA256: strings.Repeat("a", 64), Size: 1},
		},
		Images: map[string]update.Image{
			"beagle": {File: "beagle.img", SHA256: hex.EncodeToString(sum[:]), Size: int64(len(img)), Layout: 2},
		},
	}
}

// newFakeImageService builds the command's service over the fake board.
func newFakeImageService(t *testing.T, b *fakeBoard, srv *imageServer, pub ed25519.PublicKey) *imageService {
	t.Helper()
	svc := newImageService(b.req, "v1.0.0", srv.URL+"/latest/download/manifest.json", pub, nil,
		slog.New(slog.NewTextHandler(os.Stderr, &slog.HandlerOptions{Level: slog.LevelError})))
	if svc == nil {
		t.Fatal("the service is nil with -image-update set")
	}
	return svc
}

func TestImageServiceDisabled(t *testing.T) {
	if svc := newImageService(imageRequest{}, "v1.0.0", "", nil, nil, nil); svc != nil {
		t.Fatal("an empty -image-update must disable the feature")
	}
}

func TestImageServiceInstallAndRollback(t *testing.T) {
	pub, priv := updateKeys(t)
	b := newFakeBoard(t)
	img := []byte(strings.Repeat("image-bytes", 512))
	srv := newImageServer(t, priv, imageManifest(t, "v1.1.0", img), img)
	svc := newFakeImageService(t, b, srv, pub)

	st := svc.Status(context.Background())
	if !st.Enabled || !st.Ready || st.Slot != "A" || st.OtherSlot != "B" {
		t.Fatalf("status before the check: %+v", st)
	}

	// Install: writes the inactive slot, moves the boot file, does not
	// reboot by itself (main restarts after the response).
	st, err := svc.Install(context.Background())
	if err != nil {
		t.Fatalf("Install: %v", err)
	}
	if srv.gets.Load() != 1 {
		t.Fatalf("image downloads = %d, want 1", srv.gets.Load())
	}
	written, err := os.ReadFile(filepath.Join(b.dir, "mmcblk0p3"))
	if err != nil {
		t.Fatal(err)
	}
	if string(written) != string(img) {
		t.Fatal("the inactive slot does not hold the downloaded image")
	}
	if !strings.Contains(b.bootFile(), "root=/dev/mmcblk0p3 ro") {
		t.Fatalf("boot file after install: %q", b.bootFile())
	}
	if !strings.Contains(b.bootFile(), "console=ttyO0,115200n8") {
		t.Fatal("the boot file lost arguments")
	}
	if !st.Switched {
		t.Fatalf("status after install does not say the slot was switched: %+v", st)
	}
	if running, _ := os.ReadFile(filepath.Join(b.dir, "mmcblk0p2")); len(running) != 0 {
		t.Fatal("the running slot was written")
	}

	// The device reboots into B; the health timer confirms the slot.
	b.switchTo("mmcblk0p3")
	if err := svc.svc.Confirm(); err != nil {
		t.Fatalf("Confirm: %v", err)
	}
	st = svc.Status(context.Background())
	if !st.CanRollback || st.RollbackTo != "A" {
		t.Fatalf("status on B after a confirmed update: %+v", st)
	}

	// "Back to the previous image" moves the boot file back.
	if _, err := svc.Rollback(context.Background()); err != nil {
		t.Fatalf("Rollback: %v", err)
	}
	if !strings.Contains(b.bootFile(), "root=/dev/mmcblk0p2 ro") {
		t.Fatalf("boot file after rollback: %q", b.bootFile())
	}
}

func TestImageServiceRefusesBusyDevice(t *testing.T) {
	pub, priv := updateKeys(t)
	b := newFakeBoard(t)
	img := []byte("img")
	srv := newImageServer(t, priv, imageManifest(t, "v1.1.0", img), img)
	svc := newImageService(b.req, "v1.0.0", srv.URL+"/latest/download/manifest.json", pub,
		func() bool { return true }, slog.Default())
	if _, err := svc.Install(context.Background()); err == nil || !strings.Contains(err.Error(), "voice note") {
		t.Fatalf("Install while busy: %v", err)
	}
	if srv.gets.Load() != 0 {
		t.Fatal("a busy device downloaded an image")
	}
}

func TestImageServiceOldLayout(t *testing.T) {
	pub, _ := updateKeys(t)
	b := newFakeBoard(t)
	if err := os.Remove(b.req.Layout); err != nil {
		t.Fatal(err)
	}
	svc := newImageService(b.req, "v1.0.0", "http://127.0.0.1:1/manifest.json", pub, nil, slog.Default())
	st := svc.Status(context.Background())
	if st.Ready {
		t.Fatalf("a device without a layout file reports ready: %+v", st)
	}
	if !strings.Contains(st.LayoutError, "reflash once") {
		t.Fatalf("layout error = %q", st.LayoutError)
	}
	if _, err := svc.Install(context.Background()); err == nil {
		t.Fatal("Install succeeded on a one-root layout")
	}
}

func TestImageServiceRefusesNewerLayoutBeforeDownload(t *testing.T) {
	pub, priv := updateKeys(t)
	b := newFakeBoard(t)
	img := []byte("img")
	m := imageManifest(t, "v1.1.0", img)
	e := m.Images["beagle"]
	e.Layout = imageupdate.LayoutVersion + 1
	m.Images["beagle"] = e
	srv := newImageServer(t, priv, m, img)
	svc := newFakeImageService(t, b, srv, pub)
	if _, err := svc.Install(context.Background()); err == nil || !strings.Contains(err.Error(), "reflash") {
		t.Fatalf("Install with a newer layout: %v", err)
	}
	if srv.gets.Load() != 0 {
		t.Fatal("200 MB were downloaded for an image the layout cannot take")
	}
}

func TestImageServiceBadImage(t *testing.T) {
	pub, priv := updateKeys(t)
	b := newFakeBoard(t)
	img := []byte("good-image")
	m := imageManifest(t, "v1.1.0", img)
	srv := newImageServer(t, priv, m, []byte("other-image"))
	svc := newFakeImageService(t, b, srv, pub)
	if _, err := svc.Install(context.Background()); err == nil {
		t.Fatal("an image with the wrong content was installed")
	}
	if !strings.Contains(b.bootFile(), "root=/dev/mmcblk0p2 ro") {
		t.Fatalf("a failed install switched the boot file: %q", b.bootFile())
	}
}

func TestImageBootCountsAndFallsBack(t *testing.T) {
	b := newFakeBoard(t)
	img := []byte("image")
	if _, err := writeImage(b, img); err != nil {
		t.Fatal(err)
	}
	if err := switchImage(t, b); err != nil {
		t.Fatal(err)
	}
	b.switchTo("mmcblk0p3") // the reboot into the new slot

	// The main path lets BootIn remount /boot: here it already is writable.
	var acts []imageupdate.BootAction
	for i := 0; i < imageupdate.MaxBootTries; i++ {
		acts = append(acts, imageBoot(b.req, slog.Default(), time.Now()))
	}
	for i, act := range acts[:len(acts)-1] {
		if act.Reboot {
			t.Fatalf("boot %d asked for a reboot too early", i+1)
		}
	}
	if !acts[len(acts)-1].Reboot {
		t.Fatalf("the last start did not fall back: %+v", acts[len(acts)-1])
	}
	if !strings.Contains(b.bootFile(), "root=/dev/mmcblk0p2 ro") {
		t.Fatalf("the fallback did not move the boot file back: %q", b.bootFile())
	}
}

func TestImageBootOffWithoutFlag(t *testing.T) {
	act := imageBoot(imageRequest{}, slog.Default(), time.Now())
	if act.Note != "off" || act.Reboot {
		t.Fatalf("a device without the flag did something: %+v", act)
	}
}

func TestImageBootWithoutLayout(t *testing.T) {
	req := imageRequest{Dir: t.TempDir(), Layout: filepath.Join(t.TempDir(), "missing.json"),
		SysBlock: "/sys/class/block", DevDir: "/dev/"}
	act := imageBoot(req, slog.Default(), time.Now())
	if act.Reboot || act.Note != "no layout" {
		t.Fatalf("a missing layout file: %+v", act)
	}
}

func TestRemountBootReportsFailure(t *testing.T) {
	// A directory mount(8) cannot remount: the error has to name the mount.
	err := remountBoot(filepath.Join(t.TempDir(), "nope"), true)
	if err == nil {
		t.Skip("mount is not available or remounted something")
	}
	if !strings.Contains(err.Error(), "remount,rw") {
		t.Fatalf("error %q does not say what was attempted", err)
	}
}

// helpers used by the two install helpers above

func writeImage(b *fakeBoard, img []byte) (string, error) {
	l, err := imageLayout(b.req)
	if err != nil {
		return "", err
	}
	svc := &imageupdate.Service{Layout: l}
	sum := sha256.Sum256(img)
	_, err = svc.Install(context.Background(), strings.NewReader(string(img)), int64(len(img)), hex.EncodeToString(sum[:]))
	return hex.EncodeToString(sum[:]), err
}

func switchImage(t *testing.T, b *fakeBoard) error {
	t.Helper()
	l, err := imageLayout(b.req)
	if err != nil {
		return err
	}
	return (&imageupdate.Service{Layout: l}).Switch("v1.1.0")
}

// noopBootRW is the boot partition scope for a test: the temp directory is
// already writable, so entering and leaving it do nothing.
func noopBootRW() (func() error, error) { return func() error { return nil }, nil }

// statDev reads the device number of a file on Linux.
func statDev(fi os.FileInfo) (uint64, bool) {
	type devear interface{ Dev() uint32 }
	switch st := fi.Sys().(type) {
	case *syscall.Stat_t:
		return uint64(st.Dev), true
	case interface{ Dev() uint64 }:
		return st.Dev(), true
	case interface{ Dev() uint32 }:
		return uint64(st.Dev()), true
	}
	return 0, false
}
