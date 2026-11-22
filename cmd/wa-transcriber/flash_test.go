package main

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"testing"

	"github.com/emdioh/wa-transcriber/internal/flash"
)

// flashBoard is the install as files: a card (mmcblk0) with three system
// partitions, an eMMC (mmcblk1), the sysfs entries that tell them apart and a
// boot directory. The card's partitions fit 8 KiB each, which is enough for
// the region hashes and far below the eMMC.
type flashBoard struct {
	t     *testing.T
	dir   string
	img   imageRequest
	paths flashPaths
	card  int64 // bytes of each system partition on the card
}

func newFlashBoard(t *testing.T) *flashBoard {
	t.Helper()
	dir := t.TempDir()
	b := &flashBoard{t: t, dir: dir, card: 8 << 10}
	dev := dir + "/"
	boot := filepath.Join(dir, "boot")
	mustCreate(t, boot)
	b.img = imageRequest{
		Dir:      filepath.Join(dir, "data"),
		Layout:   filepath.Join(dir, "image.json"),
		SysBlock: filepath.Join(dir, "sys"),
		DevDir:   dev,
		Root:     "", // set below
		BootRW:   noopBootRW,
	}
	mustCreate(t, b.img.Dir)
	// The running root is mmcblk0p2, found by device number like a board.
	rootPart := filepath.Join(dir, "mmcblk0p2")
	writeFilled(t, rootPart, b.card, 0x11)
	b.img.Root = rootPart
	// p1 and p3 exist; p1 is the boot partition (small), p3 the other slot.
	writeFilled(t, filepath.Join(dir, "mmcblk0p1"), 4<<10, 0x22)
	writeFilled(t, filepath.Join(dir, "mmcblk0p3"), b.card, 0x33)
	// The card's whole disk: 24 KiB of partitions, but a readable device.
	writeFilled(t, filepath.Join(dir, "mmcblk0"), 24<<10, 0x44)
	// The eMMC: not removable, bigger, and it is not the running disk.
	writeFilled(t, filepath.Join(dir, "mmcblk1"), 256<<10, 0x00)
	b.sys("mmcblk0", "MMC", "1", 24<<10, "179:0")
	b.sys("mmcblk1", "MMC", "0", 256<<10, "179:16")
	b.part("mmcblk0p1", "179:1")
	b.part("mmcblk0p2", "179:2")
	b.part("mmcblk0p3", "179:3")
	// The sysfs dev of p2 is the device of the file it stands for.
	fi, err := os.Stat(rootPart)
	if err != nil {
		t.Fatal(err)
	}
	devNum, ok := statDev(fi)
	if !ok {
		t.Fatal("no device numbers on this platform")
	}
	major := devNum >> 8 & 0xfff
	minor := (devNum>>12)<<8 | devNum&0xff
	b.partDev("mmcblk0p2", strconv.FormatUint(major, 10)+":"+strconv.FormatUint(minor, 10))

	// The target's boot partition carries the boot file the clone copied,
	// naming the card's disk as the card's does.
	mustCreate(t, b.tboot())
	mustWrite(t, filepath.Join(b.tboot(), "extlinux.conf"),
		[]byte("append root="+filepath.Join(dir, "mmcblk0")+"p2 ro rootwait\n"))
	settings, _ := json.Marshal(map[string]any{
		"board": "beagle", "boot_file": filepath.Join(boot, "extlinux.conf"), "slots": []string{"p2", "p3"},
	})
	mustWrite(t, b.img.Layout, settings)
	mounts := filepath.Join(dir, "mounts")
	mustWrite(t, mounts, nil)
	b.paths = flashPaths{
		SysBlock:  b.img.SysBlock,
		DevDir:    dev,
		BootDir:   boot,
		DataDir:   b.img.Dir,
		Mounts:    mounts,
		MountFunc: b.fakeMount,
	}
	return b
}

func (b *flashBoard) sys(name, typ, removable string, size int64, dev string) {
	base := filepath.Join(b.img.SysBlock, name)
	mustCreate(b.t, filepath.Join(base, "device"))
	mustWrite(b.t, filepath.Join(base, "device", "type"), []byte(typ+"\n"))
	mustWrite(b.t, filepath.Join(base, "removable"), []byte(removable+"\n"))
	mustWrite(b.t, filepath.Join(base, "size"), []byte(strconv.FormatInt(size/512, 10)+"\n"))
	mustWrite(b.t, filepath.Join(base, "dev"), []byte(dev+"\n"))
}

func (b *flashBoard) part(name, dev string) {
	base := filepath.Join(b.img.SysBlock, name)
	mustCreate(b.t, base)
	mustWrite(b.t, filepath.Join(base, "partition"), []byte(name[len(name)-1:]+"\n"))
	mustWrite(b.t, filepath.Join(base, "dev"), []byte(dev+"\n"))
}

func (b *flashBoard) partDev(name, dev string) {
	mustWrite(b.t, filepath.Join(b.img.SysBlock, name, "dev"), []byte(dev+"\n"))
}

func (b *flashBoard) start() (string, string) {
	stdout, stderr := &bytes.Buffer{}, &bytes.Buffer{}
	code := runFlash(context.Background(), b.img, b.paths, "", "mmcblk1", stdout, stderr)
	if code != 0 {
		return stdout.String() + "\n" + stderr.String(), stderr.String()
	}
	return stdout.String(), stderr.String()
}

func mustCreate(t *testing.T, path string) {
	t.Helper()
	if err := os.MkdirAll(path, 0o755); err != nil {
		t.Fatal(err)
	}
}

func mustWrite(t *testing.T, path string, b []byte) {
	t.Helper()
	if err := os.WriteFile(path, b, 0o644); err != nil {
		t.Fatal(err)
	}
}

// tboot is the directory the console tests' mount seam stands for: the
// target's boot partition, <disk>p1.
func (b *flashBoard) tboot() string { return filepath.Join(b.dir, "wat-stage") }

// fakeMount is the console tests' stand-in for mount(8): bringing the target's
// boot partition up links the mount point to the directory standing for it, so
// the staging writes into something a test can read; "remount,ro" removes the
// link. Only the target's p1 is accepted, which is what an install may touch.
func (b *flashBoard) fakeMount(src, dir, opts string) error {
	if err := os.RemoveAll(dir); err != nil {
		return err
	}
	if src == "" || opts == "remount,ro" {
		return nil
	}
	target := src
	if rest, ok := strings.CutPrefix(src, "vfat:"); ok {
		target = rest
	}
	if target != filepath.Join(b.dir, "mmcblk1p1") {
		return fmt.Errorf("mount: %s is not the target's boot partition", src)
	}
	if err := os.MkdirAll(b.tboot(), 0o755); err != nil {
		return err
	}
	return os.Symlink(b.tboot(), dir)
}

func writeFilled(t *testing.T, path string, size int64, fill byte) {
	t.Helper()
	b := make([]byte, size)
	for i := range b {
		b[i] = fill + byte(i)
	}
	mustWrite(t, path, b)
}

func TestRunFlashInstallsAndSaysWhatToDo(t *testing.T) {
	b := newFlashBoard(t)
	// The running /data: settings and a session, no wifi.json.
	mustWrite(t, filepath.Join(b.img.Dir, "config.json"), []byte(`{"language":"it"}`))
	mustWrite(t, filepath.Join(b.img.Dir, "session.db"), []byte("session"))
	mustWrite(t, filepath.Join(b.img.Dir, "session.db-wal"), []byte("wal"))
	out, errOut := b.start()

	// The eMMC now holds the card's MBR, boot and both roots at the same
	// offsets; the MBR is the first sector of mmcblk0.
	card := readFile(t, filepath.Join(b.dir, "mmcblk0"))
	emmec := readFile(t, filepath.Join(b.dir, "mmcblk1"))
	regions := flash.Regions(512, 4<<10, 8<<10, 8<<10)
	need := flash.Need(regions)
	if !bytes.Equal(emmec[:need], card[:need]) {
		t.Fatal("the internal storage does not hold the card's regions")
	}
	if emmec[need] != byte(need) {
		t.Fatal("the clone wrote past the regions")
	}
	for _, want := range []string{"sha256", "REMOVE THE SD CARD", "internal storage"} {
		if !bytes.Contains([]byte(out), []byte(want)) {
			t.Fatalf("console output missing %q:\n%s", want, out)
		}
	}
	if !bytes.Contains([]byte(errOut), []byte("%")) {
		t.Fatalf("no progress on stderr:\n%s", errOut)
	}
	// The marker and the files are on the *target's* boot partition: the
	// card's /boot is not where the first eMMC boot reads them from.
	if _, err := os.Stat(filepath.Join(b.paths.BootDir, flash.MarkerName)); !errors.Is(err, os.ErrNotExist) {
		t.Fatal("the marker landed on the card's boot partition")
	}
	marker := string(readFile(t, filepath.Join(b.tboot(), flash.MarkerName)))
	if !bytes.Contains([]byte(marker), []byte("file session.db")) ||
		!bytes.Contains([]byte(marker), []byte("file session.db-wal")) ||
		bytes.Contains([]byte(marker), []byte("file wifi.json")) {
		t.Fatalf("marker:\n%s", marker)
	}
	if _, err := os.Stat(filepath.Join(b.tboot(), flash.MigrateDir, "session.db-wal")); err != nil {
		t.Fatalf("session.db-wal was not staged: %v", err)
	}
	if !bytes.Contains([]byte(out), []byte("wat-migrate.d")) {
		t.Fatalf("the console does not say where the session was staged:\n%s", out)
	}
}

func TestRunFlashRefusesWithoutConfirmation(t *testing.T) {
	b := newFlashBoard(t)
	before := readFile(t, filepath.Join(b.dir, "mmcblk1"))
	out, errOut := &bytes.Buffer{}, &bytes.Buffer{}
	code := runFlash(context.Background(), b.img, b.paths, "", "", out, errOut)
	if code != 2 {
		t.Fatalf("exit %d, want 2", code)
	}
	if !bytes.Contains(errOut.Bytes(), []byte("-flash-confirm")) {
		t.Fatalf("the refusal does not say how to confirm:\n%s", errOut)
	}
	if !bytes.Equal(before, readFile(t, filepath.Join(b.dir, "mmcblk1"))) {
		t.Fatal("an unconfirmed run wrote the destination")
	}
	if _, err := os.Stat(filepath.Join(b.tboot(), flash.MarkerName)); !errors.Is(err, os.ErrNotExist) {
		t.Fatal("an unconfirmed run wrote the migration marker")
	}
}

func TestRunFlashRefusesANonTarget(t *testing.T) {
	b := newFlashBoard(t)
	out, errOut := &bytes.Buffer{}, &bytes.Buffer{}
	if code := runFlash(context.Background(), b.img, b.paths, filepath.Join(b.dir, "mmcblk0"), "mmcblk0", out, errOut); code != 2 {
		t.Fatalf("cloning onto the running card: exit %d, want 2", code)
	}
	if !bytes.Contains(errOut.Bytes(), []byte("booted from")) {
		t.Fatalf("the refusal does not say it is the running disk:\n%s", errOut)
	}
	if code := runFlash(context.Background(), b.img, b.paths, "/dev/sdz", "sdz", out, errOut); code != 2 {
		t.Fatalf("unknown device: exit %d, want 2", code)
	}
}

func TestRunFlashWithoutInternalFlash(t *testing.T) {
	b := newFlashBoard(t)
	// The Pi 2: the card is the only MMC disk, and it is removable.
	if err := os.RemoveAll(filepath.Join(b.img.SysBlock, "mmcblk1")); err != nil {
		t.Fatal(err)
	}
	if err := os.Remove(filepath.Join(b.dir, "mmcblk1")); err != nil {
		t.Fatal(err)
	}
	out, errOut := &bytes.Buffer{}, &bytes.Buffer{}
	if code := runFlash(context.Background(), b.img, b.paths, "", "", out, errOut); code != 2 {
		t.Fatalf("exit %d, want 2", code)
	}
	if !bytes.Contains(errOut.Bytes(), []byte("no internal storage found")) {
		t.Fatalf("the refusal is not the one line:\n%s", errOut)
	}
}

func TestFlashServiceStatusAndRefusals(t *testing.T) {
	b := newFlashBoard(t)
	svc := newFlashService(b.img, b.paths, nil)
	if svc == nil {
		t.Fatal("no service with -image-update set")
	}
	st := svc.Status(context.Background())
	if st.Error != "" {
		t.Fatalf("status error: %s", st.Error)
	}
	if st.Target != filepath.Join(b.dir, "mmcblk1") || st.Size != 256<<10 || st.Install != "mmcblk1" {
		t.Fatalf("status = %+v", st)
	}
	if st.Marker != flashMarkerName || st.Need <= 0 {
		t.Fatalf("status = %+v", st)
	}
	// The service only reports: it never writes without the confirmation.
	before := readFile(t, filepath.Join(b.dir, "mmcblk1"))
	if _, err := svc.Install(context.Background(), "yes"); !errors.Is(err, flash.ErrNoConfirm) {
		t.Fatalf("err = %v, want ErrNoConfirm", err)
	}
	if !bytes.Equal(before, readFile(t, filepath.Join(b.dir, "mmcblk1"))) {
		t.Fatal("an unconfirmed install wrote the destination")
	}
	mustWrite(t, filepath.Join(b.img.Dir, "config.json"), []byte(`{}`))
	if _, err := svc.Install(context.Background(), "mmcblk1"); err != nil {
		t.Fatal(err)
	}
	if _, err := os.Stat(filepath.Join(b.tboot(), flash.MarkerName)); err != nil {
		t.Fatalf("the marker was not written on the target's boot partition: %v", err)
	}
}

func TestFlashServiceOneLineWithoutInternalFlash(t *testing.T) {
	b := newFlashBoard(t)
	if err := os.RemoveAll(filepath.Join(b.img.SysBlock, "mmcblk1")); err != nil {
		t.Fatal(err)
	}
	svc := newFlashService(b.img, b.paths, nil)
	st := svc.Status(context.Background())
	if st.Error == "" || !bytes.Contains([]byte(st.Error), []byte("no internal storage")) {
		t.Fatalf("a board without internal flash says %q", st.Error)
	}
	if st.Target != "" {
		t.Fatalf("a target was reported anyway: %+v", st)
	}
}

func TestFlashServiceOffWithoutImageUpdate(t *testing.T) {
	b := newFlashBoard(t)
	b.img.Dir = ""
	if svc := newFlashService(b.img, b.paths, nil); svc != nil {
		t.Fatal("the card is built without the board layout file")
	}
}

func readFile(t *testing.T, path string) []byte {
	t.Helper()
	b, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	return b
}
