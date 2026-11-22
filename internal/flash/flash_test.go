package flash

import (
	"context"
	"errors"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"testing"
)

// fixture is a machine in a temp directory: a sysfs tree with a removable SD
// card (the running disk) and a fixed eMMC, device nodes as regular files
// standing in for the block devices, and a /boot directory.
type fixture struct {
	dir     string
	sys     string
	dev     string
	boot    string
	data    string // the running /data the session is staged from
	tboot   string // the target's boot partition, where the mount seam puts it
	card    string // /dev-like path of the running card
	emmec   string // ... of the internal storage
	mounts  string
	source  string // the card's image bytes
	regions []Region
	// mounts holds every mount call the install made, in order.
	mounts_ []string
	// reread holds the disks whose partition table the install re-read, in
	// order, and rereadErr is what that seam returns.
	reread_   []string
	rereadErr error
	// umounts records the staging mount points the install detached.
	umounts []string
}

// bootFileRel is the boot file's path inside the boot partition of the
// fixture, the same shape as the BeagleBone Black's.
const bootFileRel = "extlinux/extlinux.conf"

// newFixture builds a card with a 4 KiB MBR region and three 8 KiB
// partitions, a 4 MiB eMMC disk and the sysfs entries that describe both.
func newFixture(t *testing.T) *fixture {
	t.Helper()
	dir := t.TempDir()
	f := &fixture{
		dir:   dir,
		sys:   filepath.Join(dir, "sys"),
		dev:   filepath.Join(dir, "dev") + "/",
		boot:  filepath.Join(dir, "boot"),
		data:  filepath.Join(dir, "data"),
		tboot: filepath.Join(dir, "target-boot"),
	}
	f.mounts = filepath.Join(dir, "mounts")
	if err := os.MkdirAll(f.dev, 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.MkdirAll(f.boot, 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.MkdirAll(f.data, 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.MkdirAll(filepath.Join(f.tboot, "extlinux"), 0o755); err != nil {
		t.Fatal(err)
	}
	// The boot file the clone copied verbatim: it names the *card's* disk.
	mustErr(os.WriteFile(filepath.Join(f.tboot, bootFileRel),
		[]byte("label wa\n    kernel /zImage\n    append root=/dev/mmcblk0p2 ro rootwait\n"), 0o644))
	// The running /data: the settings and a session with its WAL, and no
	// wifi.json (a device that never joined a network).
	mustErr(os.WriteFile(filepath.Join(f.data, "config.json"), []byte(`{"language":"it"}`), 0o600))
	mustErr(os.WriteFile(filepath.Join(f.data, "session.db"), []byte("sqlite-header-and-a-few-pages"), 0o600))
	mustErr(os.WriteFile(filepath.Join(f.data, "session.db-wal"), []byte("the-bulk-of-the-session"), 0o600))
	// The running card: the SD slot, removable, 64 KiB of image.
	f.card = f.dev + "mmcblk0"
	f.mkdisk("mmcblk0", "MMC", true, 64<<10)
	f.source = f.writeDisk(f.card, 64<<10, 0x5a)
	f.regions = Regions(512, 8<<10, 8<<10, 8<<10)
	// The eMMC: not removable, 4 MiB and empty.
	f.emmec = f.dev + "mmcblk1"
	f.mkdisk("mmcblk1", "MMC", false, 4<<20)
	// The target's boot partition: <disk>p1, as the BBB names it. It exists so
	// a mount of it can succeed; the fixture's mount seam hands the staging a
	// directory of its own (see mount).
	mustErr(os.WriteFile(f.dev+"mmcblk1p1", nil, 0o644))
	// The marker lives next to the card's files; the mounts file starts empty.
	if err := os.WriteFile(f.mounts, nil, 0o644); err != nil {
		t.Fatal(err)
	}
	return f
}

// mkdisk creates the sysfs entry of a whole disk plus one partition, and the
// device nodes of both.
func (f *fixture) mkdisk(name, typ string, removable bool, size int64) {
	base := filepath.Join(f.sys, name)
	mustErr(os.MkdirAll(filepath.Join(base, "device"), 0o755))
	mustErr(os.WriteFile(filepath.Join(base, "device", "type"), []byte(typ+"\n"), 0o644))
	flag := "0\n"
	if removable {
		flag = "1\n"
	}
	mustErr(os.WriteFile(filepath.Join(base, "removable"), []byte(flag), 0o644))
	mustErr(os.WriteFile(filepath.Join(base, "size"), []byte(itoa(size/512)+"\n"), 0o644))
	mustErr(os.WriteFile(filepath.Join(base, "dev"), []byte("179:0\n"), 0o644))
	if _, err := os.Stat(f.dev + name); err != nil {
		f.writeDisk(f.dev+name, size, 0)
	}
	// p1, so the running root can be "on" one of them.
	part := filepath.Join(f.sys, name+"p1")
	mustErr(os.MkdirAll(part, 0o755))
	mustErr(os.WriteFile(filepath.Join(part, "partition"), []byte("1\n"), 0o644))
	mustErr(os.WriteFile(filepath.Join(part, "dev"), []byte("179:1\n"), 0o644))
}

func (f *fixture) writeDisk(path string, size int64, fill byte) string {
	b := make([]byte, size)
	for i := range b {
		b[i] = fill + byte(i*7)
	}
	mustErr(os.WriteFile(path, b, 0o644))
	return string(b)
}

func must(t *testing.T, err error) {
	t.Helper()
	if err != nil {
		t.Fatal(err)
	}
}

// mustErr is must for the helper that has no *testing.T.
func mustErr(err error) {
	if err != nil {
		panic(err)
	}
}

func itoa(n int64) string {
	if n == 0 {
		return "0"
	}
	var b []byte
	for n > 0 {
		b = append([]byte{byte('0' + n%10)}, b...)
		n /= 10
	}
	return string(b)
}

// read returns the file contents.
func read(t *testing.T, path string) []byte {
	t.Helper()
	b, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	return b
}

func (f *fixture) sysOf() Sys {
	return Sys{SysBlock: f.sys, DevDir: f.dev, Root: f.boot} // root = an empty dir: stat is not on any disk
}

func (f *fixture) request() Request {
	return Request{
		Sys:        f.sysOf(),
		Source:     f.card,
		SourceName: "mmcblk0",
		Sizes:      [3]int64{8 << 10, 8 << 10, 8 << 10},
		Target:     f.emmec,
		Confirm:    "mmcblk1",
		BootDir:    f.boot,
		DataDir:    f.data,
		Mounts:     f.mounts,
		BootFile:   bootFileRel,
		MountFunc:  f.mount,
		RereadFunc: f.reread,
		UmountFunc: f.umount,
	}
}

// umount is the detach seam: it records the mount point and drops the
// fixture's symlink (nothing is really mounted in a test).
func (f *fixture) umount(dir string) error {
	f.umounts = append(f.umounts, dir)
	if fi, err := os.Lstat(dir); err == nil && fi.Mode()&os.ModeSymlink != 0 {
		return os.Remove(dir)
	}
	return nil
}

// reread is the partition-table seam: it records the disk it was asked about.
func (f *fixture) reread(disk string) error {
	f.reread_ = append(f.reread_, disk)
	return f.rereadErr
}

// mount is the mount seam of the fixture: the mount root is the fixture's
// temp directory, the staging directory stands for the target's boot
// partition, and its contents *are* f.tboot from then on (there is nothing to
// mount in a test, so the seam links the two paths). Every call is recorded.
func (f *fixture) mount(src, dir, opts string) error {
	f.mounts_ = append(f.mounts_, src+" "+opts)
	if opts == "remount,ro" || src == "" {
		if fi, err := os.Lstat(dir); err == nil && fi.Mode()&os.ModeSymlink != 0 {
			return os.Remove(dir)
		}
		return nil
	}
	target := src
	if rest, ok := strings.CutPrefix(src, "vfat:"); ok {
		target = rest
	}
	if target != filepath.Join(f.dev, "mmcblk1p1") {
		return fmt.Errorf("mount: %s is not the target's boot partition", src)
	}
	if fi, err := os.Lstat(dir); err == nil && fi.Mode()&os.ModeSymlink != 0 {
		return nil
	}
	if err := os.RemoveAll(dir); err != nil {
		return err
	}
	if err := os.MkdirAll(filepath.Dir(dir), 0o755); err != nil {
		return err
	}
	return os.Symlink(f.tboot, dir)
}

// stagedDir is where the staged files and the marker end up: the target's
// boot partition, as <disk>p1 would be.
func (f *fixture) stagedDir() string { return f.tboot }

// lastMountOpts is the option string of the last mount call: the install has
// to leave the boot partition mounted read-only again.
func (f *fixture) lastMountOpts() string {
	if len(f.mounts_) == 0 {
		return ""
	}
	parts := strings.Fields(f.mounts_[len(f.mounts_)-1])
	if len(parts) < 2 {
		return ""
	}
	return parts[1]
}

func TestTargetFindsTheInternalStorage(t *testing.T) {
	f := newFixture(t)
	got, err := f.sysOf().Target()
	if err != nil {
		t.Fatal(err)
	}
	if got.Device != f.emmec || got.Size != 4<<20 || got.Removable {
		t.Fatalf("target = %+v", got)
	}
}

func TestTargetRefusesWithoutInternalFlash(t *testing.T) {
	f := newFixture(t)
	// A board whose only MMC disk is the card (the Pi 2): mark it running by
	// making it the root. A file cannot be a disk, so the fixture's root is
	// the fixture directory and the running disk is found through the
	// partition's dev number: on Linux stat of the dir is not 179:1, so the
	// card would look like a candidate. Say it is running directly.
	must(t, os.Remove(filepath.Join(f.sys, "mmcblk1", "device", "type")))
	must(t, os.RemoveAll(filepath.Join(f.sys, "mmcblk1")))
	// The card is the running disk: removable=1 refuses it first on a board,
	// which is why a Pi 2 answers "no internal storage" and not "ambiguous".
	_, err := f.sysOf().Target()
	if !errors.Is(err, ErrNoTarget) {
		t.Fatalf("err = %v, want ErrNoTarget", err)
	}
	if !strings.Contains(err.Error(), "no internal storage found") {
		t.Fatalf("err = %v", err)
	}
	if !strings.Contains(err.Error(), "removable") {
		t.Fatalf("the refusal does not say what it saw: %v", err)
	}
}

func TestTargetRefusesAmbiguous(t *testing.T) {
	f := newFixture(t)
	f.mkdisk("mmcblk2", "MMC", false, 4<<20)
	_, err := f.sysOf().Target()
	if err == nil || errors.Is(err, ErrNoTarget) {
		t.Fatalf("two candidates must be refused, got %v", err)
	}
	for _, want := range []string{"2 internal storage devices", "mmcblk1", "mmcblk2"} {
		if !strings.Contains(err.Error(), want) {
			t.Fatalf("refusal missing %q: %v", want, err)
		}
	}
}

func TestTargetIgnoresNonMMCAndSmallDisks(t *testing.T) {
	f := newFixture(t)
	f.mkdisk("sda", "SCSI", false, 8<<20)
	f.mkdisk("mmcblk3", "MMC", false, 1<<10) // too small for the 24.5 KiB copy
	sys := f.sysOf()
	sys.Need = Need(f.regions)
	got, err := sys.Target()
	if err != nil {
		t.Fatal(err)
	}
	if got.Name != "mmcblk1" {
		t.Fatalf("target = %+v", got)
	}
}

func TestTargetRefusesRunningDisk(t *testing.T) {
	f := newFixture(t)
	// Pretend the fixture directory (the "root") is on mmcblk0p1: rewrite the
	// partition's dev to the dir's own device number.
	fi, err := os.Stat(f.boot)
	if err != nil {
		t.Fatal(err)
	}
	dev, ok := fsDevice(fi)
	if !ok {
		t.Skip("no device numbers on this platform")
	}
	major, minor := dev>>8, dev&0xff|((dev>>12)&0xfff00)
	name := fmt.Sprintf("%d:%d", major, minor)
	must(t, os.WriteFile(filepath.Join(f.sys, "mmcblk0p1", "dev"), []byte(name+"\n"), 0o644))
	// Now mmcblk0 is the running disk: the only other MMC disk is mmcblk1.
	got, err := f.sysOf().Target()
	if err != nil {
		t.Fatal(err)
	}
	if got.Name != "mmcblk1" {
		t.Fatalf("target = %+v", got)
	}
	// Asking for the running disk by name is refused as such.
	req := f.request()
	req.Target = f.card
	_, err = Plan(req)
	if !errors.Is(err, ErrSameDisk) {
		t.Fatalf("err = %v, want ErrSameDisk", err)
	}
}

func TestRegionsAreContiguous(t *testing.T) {
	r := Regions(512, 32<<20, 160<<20, 160<<20)
	if len(r) != 4 || r[0].Name != "mbr" || r[3].Name != "root-b" {
		t.Fatalf("regions = %v", r)
	}
	if Need(r) != 512+352<<20 {
		t.Fatalf("need = %d", Need(r))
	}
	if err := checkRegions(r); err != nil {
		t.Fatal(err)
	}
	bad := []Region{{Name: "a", Offset: 0, Size: 4096}, {Name: "b", Offset: 2048, Size: 4096}}
	if err := checkRegions(bad); err == nil {
		t.Fatal("overlapping regions accepted")
	}
}

func TestCloneCopiesAndVerifies(t *testing.T) {
	f := newFixture(t)
	src, err := os.Open(f.card)
	if err != nil {
		t.Fatal(err)
	}
	defer src.Close()
	dst, err := os.OpenFile(f.emmec, os.O_RDWR, 0)
	if err != nil {
		t.Fatal(err)
	}
	defer dst.Close()
	var last, total int64
	regions, err := Clone(context.Background(), src, dst, f.regions, func(done, tot int64) {
		last, total = done, tot
	})
	if err != nil {
		t.Fatal(err)
	}
	if last != total || total != 2*Need(f.regions) {
		t.Fatalf("progress ended at %d of %d", last, total)
	}
	for _, r := range regions {
		if len(r.SHA256) != 64 {
			t.Fatalf("region %s has no hash", r.Name)
		}
	}
	// Byte for byte, at the same offsets, and the rest of the disk untouched.
	want := read(t, f.card)
	got := read(t, f.emmec)
	if string(got[:Need(f.regions)]) != string(want[:Need(f.regions)]) {
		t.Fatal("the eMMC does not hold the card's regions")
	}
	if got[Need(f.regions)] != byte(Need(f.regions)) {
		t.Fatal("the clone wrote past the regions")
	}
}

func TestCloneNeedsAReadableDestination(t *testing.T) {
	f := newFixture(t)
	src, err := os.Open(f.card)
	if err != nil {
		t.Fatal(err)
	}
	defer src.Close()
	w, err := os.OpenFile(f.emmec, os.O_WRONLY, 0)
	if err != nil {
		t.Fatal(err)
	}
	defer w.Close()
	if _, err := Clone(context.Background(), src, w, f.regions, nil); err == nil {
		t.Fatal("a write-only destination was accepted: the copy cannot be verified")
	}
}

func TestCloneRefusesShortSource(t *testing.T) {
	f := newFixture(t)
	// A source that ends inside root-b: the card was swapped or shrunk.
	short := filepath.Join(f.dir, "short")
	must(t, os.WriteFile(short, read(t, f.card)[:Need(f.regions)-4096], 0o644))
	src, err := os.Open(short)
	if err != nil {
		t.Fatal(err)
	}
	defer src.Close()
	dst, err := os.OpenFile(f.emmec, os.O_RDWR, 0)
	if err != nil {
		t.Fatal(err)
	}
	defer dst.Close()
	_, err = Clone(context.Background(), src, dst, f.regions, nil)
	var nw *ErrNotWritten
	if !errors.As(err, &nw) {
		t.Fatalf("err = %v, want ErrNotWritten", err)
	}
	if !strings.HasPrefix(nw.Region, "root-b") {
		t.Fatalf("the failure names %q", nw.Region)
	}
}

// failingReaderAt fails after n bytes, like a card that is pulled out.
type failingReaderAt struct {
	r    io.ReaderAt
	left int64
}

func (f *failingReaderAt) ReadAt(p []byte, off int64) (int, error) {
	if int64(len(p)) > f.left {
		p = p[:max(0, f.left)]
		f.left -= int64(len(p))
		if len(p) == 0 {
			return 0, errors.New("input/output error")
		}
		n, _ := f.r.ReadAt(p, off)
		return n, errors.New("input/output error")
	}
	f.left -= int64(len(p))
	return f.r.ReadAt(p, off)
}

func TestCloneReportsTheRegionThatFailed(t *testing.T) {
	f := newFixture(t)
	src, err := os.Open(f.card)
	if err != nil {
		t.Fatal(err)
	}
	defer src.Close()
	dst, err := os.OpenFile(f.emmec, os.O_RDWR, 0)
	if err != nil {
		t.Fatal(err)
	}
	defer dst.Close()
	r := &failingReaderAt{r: src, left: 8<<10 + 512 + 100} // dies inside root-a
	_, err = Clone(context.Background(), r, dst, f.regions, nil)
	var nw *ErrNotWritten
	if !errors.As(err, &nw) {
		t.Fatalf("err = %v, want ErrNotWritten", err)
	}
	if !strings.HasPrefix(nw.Region, "root-a") {
		t.Fatalf("the failure names %q", nw.Region)
	}
}

func TestCloneCancelled(t *testing.T) {
	f := newFixture(t)
	src, err := os.Open(f.card)
	if err != nil {
		t.Fatal(err)
	}
	defer src.Close()
	dst, err := os.OpenFile(f.emmec, os.O_RDWR, 0)
	if err != nil {
		t.Fatal(err)
	}
	defer dst.Close()
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	_, err = Clone(ctx, src, dst, f.regions, nil)
	var nw *ErrNotWritten
	if !errors.As(err, &nw) || !errors.Is(err, context.Canceled) {
		t.Fatalf("err = %v, want a cancellation naming the region", err)
	}
	if !strings.HasPrefix(nw.Region, "mbr") {
		t.Fatalf("cancelled in %q", nw.Region)
	}
}

func TestVerifyCatchesACorruptWrite(t *testing.T) {
	f := newFixture(t)
	src, err := os.Open(f.card)
	if err != nil {
		t.Fatal(err)
	}
	defer src.Close()
	dst, err := os.OpenFile(f.emmec, os.O_RDWR, 0)
	if err != nil {
		t.Fatal(err)
	}
	defer dst.Close()
	regions, err := Clone(context.Background(), src, dst, f.regions, nil)
	if err != nil {
		t.Fatal(err)
	}
	// The eMMC wrote most of it but one sector of root-a came back wrong.
	off := regions[2].Offset + 4096
	if _, err := dst.WriteAt([]byte{0xff}, off); err != nil {
		t.Fatal(err)
	}
	err = Verify(context.Background(), src, dst, regions, nil)
	var nw *ErrNotWritten
	if !errors.As(err, &nw) {
		t.Fatalf("err = %v, want ErrNotWritten", err)
	}
	if !strings.HasPrefix(nw.Region, "root-a") || !strings.Contains(err.Error(), "reads back") {
		t.Fatalf("verify says %v", err)
	}
	// And the next clone fixes it: the operation is repeatable.
	src2, err := os.Open(f.card)
	if err != nil {
		t.Fatal(err)
	}
	defer src2.Close()
	if _, err := Clone(context.Background(), src2, dst, f.regions, nil); err != nil {
		t.Fatalf("second clone: %v", err)
	}
}

// corruptWriterAt writes every byte but one wrong: the "silent corruption"
// Clone's read-back exists for.
type corruptWriterAt struct{ w io.WriterAt }

func (c corruptWriterAt) WriteAt(p []byte, off int64) (int, error) {
	return c.w.WriteAt(p, off)
}

// offsetWriterAt moves every write: the destination says "done" and holds
// something else, which is what only the read-back can find.
type offsetWriterAt struct{ w io.WriterAt }

func (o offsetWriterAt) WriteAt(p []byte, off int64) (int, error) {
	return o.w.WriteAt(p, off+512)
}

func (o offsetWriterAt) ReadAt(p []byte, off int64) (int, error) {
	return o.w.(io.ReaderAt).ReadAt(p, off)
}

func TestCloneFailsWhenTheReadBackDiffers(t *testing.T) {
	f := newFixture(t)
	src, err := os.Open(f.card)
	if err != nil {
		t.Fatal(err)
	}
	defer src.Close()
	dst, err := os.OpenFile(f.emmec, os.O_RDWR, 0)
	if err != nil {
		t.Fatal(err)
	}
	defer dst.Close()
	// A destination whose writes land one sector too far: everything
	// "succeeds" and the read-back is what finds out.
	_, err = Clone(context.Background(), src, offsetWriterAt{dst}, f.regions, nil)
	var nw *ErrNotWritten
	if !errors.As(err, &nw) {
		t.Fatalf("err = %v, want ErrNotWritten", err)
	}
	if !strings.HasPrefix(nw.Region, "mbr") {
		t.Fatalf("the read-back failed in %q", nw.Region)
	}
}

func TestInstallHappyPath(t *testing.T) {
	f := newFixture(t)
	rep, err := Install(context.Background(), f.request())
	if err != nil {
		t.Fatal(err)
	}
	if !rep.Written || rep.Plan.Target.Name != "mmcblk1" || rep.Unpaired {
		t.Fatalf("report = %+v", rep)
	}
	// The marker is on the *target's* boot partition, not on the card's
	// /boot: the card is out of the slot on the first boot from the clone.
	if _, err := os.Stat(filepath.Join(f.boot, MarkerName)); err == nil {
		t.Fatal("the marker was written on the card's boot partition")
	}
	dir := f.stagedDir()
	m, err := ReadMarker(dir)
	if err != nil {
		t.Fatal(err)
	}
	if m == nil || len(m.Files) != 3 {
		t.Fatalf("marker = %+v", m)
	}
	// The old device format is still written for a reader of the old format,
	// and the new format needs no device at all.
	if m.From != "/dev/mmcblk0p4" {
		t.Fatalf("marker from = %q", m.From)
	}
	if _, err := os.Stat(filepath.Join(dir, MigrateDir, "session.db-wal")); err != nil {
		t.Fatalf("session.db-wal was not staged: %v", err)
	}
	if got := string(read(t, filepath.Join(dir, MigrateDir, "session.db"))); got != "sqlite-header-and-a-few-pages" {
		t.Fatalf("staged session.db = %q", got)
	}
	// The files land before the marker: the marker lists only what is there.
	for _, name := range m.Files {
		if _, err := os.Stat(filepath.Join(dir, MigrateDir, name)); err != nil {
			t.Fatalf("the marker lists %s, which is not staged", name)
		}
	}
	for _, name := range rep.Staged {
		if !slices.Contains(m.Files, name) {
			t.Fatalf("%s was staged but the marker does not list it", name)
		}
	}
	if len(rep.Skipped) == 0 || rep.Skipped[len(rep.Skipped)-1] != "wifi.json" {
		t.Fatalf("skipped = %v, want wifi.json (the running /data has none)", rep.Skipped)
	}
	// The boot partition was mounted read-write and left read-only again.
	if got := f.lastMountOpts(); got != "remount,ro" {
		t.Fatalf("mounts = %v", f.mounts_)
	}
	if !strings.Contains(f.mounts_[0], " rw") {
		t.Fatalf("the boot partition was never made writable: %v", f.mounts_)
	}
}

// requestFor is the fixture's request with a different body for the writable
// window on the target's boot partition.
func (f *fixture) requestFor(fn func(dir string) (Stage, error)) Request {
	req := f.request()
	req.Stage = fn
	return req
}

func TestStageFilesSkipsMissingAndWritesNoTempNames(t *testing.T) {
	src := t.TempDir()
	dst := filepath.Join(t.TempDir(), MigrateDir)
	must(t, os.WriteFile(filepath.Join(src, "config.json"), []byte("{}"), 0o600))
	must(t, os.WriteFile(filepath.Join(src, "session.db"), []byte("db"), 0o600))
	// A directory where a file should be is skipped, not copied.
	must(t, os.MkdirAll(filepath.Join(src, "session.db-shm"), 0o755))

	st, err := stageFiles(src, dst, MigrateFiles, 1<<20, 4096, nil)
	if err != nil {
		t.Fatal(err)
	}
	if strings.Join(st.Files, ",") != "config.json,session.db" {
		t.Fatalf("staged = %v", st.Files)
	}
	if strings.Join(st.Skipped, ",") != "session.db-wal,session.db-shm,wifi.json" {
		t.Fatalf("skipped = %v", st.Skipped)
	}
	// Nothing but the staged names: no half-written temp file survives.
	ents, err := os.ReadDir(dst)
	if err != nil {
		t.Fatal(err)
	}
	for _, e := range ents {
		if !safeName(e.Name()) || strings.HasPrefix(e.Name(), ".") {
			t.Fatalf("left behind: %s", e.Name())
		}
	}
	if got := string(read(t, filepath.Join(dst, "session.db"))); got != "db" {
		t.Fatalf("staged session.db = %q", got)
	}
}

func TestStageFilesRefusesWithoutSpace(t *testing.T) {
	src := t.TempDir()
	dst := filepath.Join(t.TempDir(), MigrateDir)
	must(t, os.WriteFile(filepath.Join(src, "session.db"), make([]byte, 1<<20), 0o600))
	_, err := stageFiles(src, dst, MigrateFiles, 64<<10, 4096, nil)
	if !errors.Is(err, ErrNoSpace) {
		t.Fatalf("err = %v, want ErrNoSpace", err)
	}
	if !strings.Contains(err.Error(), "free") || !strings.Contains(err.Error(), "need") {
		t.Fatalf("the refusal does not say how much is needed and how much is there: %v", err)
	}
	if _, err := os.Stat(dst); !errors.Is(err, os.ErrNotExist) {
		t.Fatal("a refused staging created the directory anyway")
	}
}

func TestInstallWithoutSpaceBootsUnpaired(t *testing.T) {
	f := newFixture(t)
	// The target's boot partition, as small as a boot partition can be.
	dir := f.stagedDir()
	rep, err := Install(context.Background(), f.requestFor(func(dir string) (Stage, error) {
		return stageFiles(f.data, filepath.Join(dir, MigrateDir), MigrateFiles, 4, 4096, nil)
	}))
	if err != nil {
		t.Fatalf("a full boot partition must not fail the clone: %v", err)
	}
	if !rep.Written || !rep.Unpaired {
		t.Fatalf("report = %+v", rep)
	}
	if !strings.Contains(rep.UnpairedWhy, "boot unpaired") || !strings.Contains(rep.UnpairedWhy, "free") {
		t.Fatalf("unpaired why = %q", rep.UnpairedWhy)
	}
	if _, err := os.Stat(filepath.Join(dir, MarkerName)); err == nil {
		t.Fatal("a marker was written with nothing staged behind it")
	}
	if _, err := os.Stat(filepath.Join(dir, MigrateDir)); err == nil {
		t.Fatal("an empty staging directory was left on the boot partition")
	}
}

func TestInstallRefusalsLeaveBothDevicesAlone(t *testing.T) {
	f := newFixture(t)
	before := string(read(t, f.emmec))
	cases := []struct {
		name   string
		modify func(*Request)
		want   error
	}{
		{"no confirmation", func(r *Request) { r.Confirm = "" }, ErrNoConfirm},
		{"wrong confirmation", func(r *Request) { r.Confirm = "yes" }, ErrNoConfirm},
		{"same disk", func(r *Request) { r.Target = f.card }, ErrSameDisk},
		{"too small", func(r *Request) {
			r.Target = f.mock("mmcblk4", false, 4096)
		}, ErrTooSmall},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			req := f.request()
			mounted := false
			req.MountFunc = func(src, dir, opts string) error {
				mounted = true
				return nil
			}
			tc.modify(&req)
			_, err := Install(context.Background(), req)
			if !errors.Is(err, tc.want) {
				t.Fatalf("err = %v, want %v", err, tc.want)
			}
			if mounted {
				t.Fatal("a refused install mounted the target's boot partition")
			}
			if string(read(t, f.emmec)) != before {
				t.Fatal("a refused install wrote to the destination")
			}
			if _, err := os.Stat(filepath.Join(f.boot, MarkerName)); err == nil {
				t.Fatal("a refused install wrote the migration marker")
			}
		})
	}
}

// mock adds a disk of a given size to the fixture and returns its node.
func (f *fixture) mock(name string, removable bool, size int64) string {
	f.mkdisk(name, "MMC", removable, size)
	path := f.dev + name
	mustErr(os.Truncate(path, size))
	return path
}

func TestInstallRefusesAMountedDestination(t *testing.T) {
	f := newFixture(t)
	must(t, os.WriteFile(f.mounts, []byte("/dev/mmcblk1p2 /mnt ext4 rw 0 0\n"), 0o644))
	req := f.request()
	_, err := Install(context.Background(), req)
	if !errors.Is(err, ErrMounted) {
		t.Fatalf("err = %v, want ErrMounted", err)
	}
	if !strings.Contains(err.Error(), "mmcblk1p2") {
		t.Fatalf("the refusal does not name the mount: %v", err)
	}
}

func TestMarkerRoundTripAndPathSafety(t *testing.T) {
	m, err := parseMarker("wat-migrate 1\nfrom /dev/mmcblk0p4\nfile config.json\nfile ../../etc/passwd\nfile ok.txt\n")
	if err != nil {
		t.Fatal(err)
	}
	if m.From != "/dev/mmcblk0p4" {
		t.Fatalf("from = %q", m.From)
	}
	if len(m.Files) != 2 || m.Files[1] != "ok.txt" {
		t.Fatalf("files = %v", m.Files)
	}
	// A marker without a device is the install's own format (the files are
	// staged on the boot partition), so it parses.
	m2, err := parseMarker("wat-migrate 1\nfile config.json\n")
	if err != nil || m2.From != "" || len(m2.Files) != 1 {
		t.Fatalf("staged-only marker = %+v, %v", m2, err)
	}
	if _, err := parseMarker("wat-migrate 1\n"); err == nil {
		t.Fatal("a marker that names nothing to copy was accepted")
	}
	m3, err := parseMarker("wat-migrate 1\nfile ../../etc/passwd\n")
	if err != nil || len(m3.Files) != 0 {
		t.Fatalf("an unsafe name was kept: %+v, %v", m3, err)
	}
	if got := NewMarker("sda").From; got != "/dev/sda4" {
		t.Fatalf("devPart of sda4 = %q", got)
	}
}

// TestInstallRereadsTheTableBeforeMountingTheTarget pins the bug the board
// found: after the clone the kernel still had the old table, so mounting
// <target>p1 read the previous layout and failed with EINVAL. The re-read has
// to happen before the staging mount, and a re-read that fails must not lose
// the verified clone.
func TestInstallRereadsTheTableBeforeMountingTheTarget(t *testing.T) {
	t.Run("before the mount", func(t *testing.T) {
		f := newFixture(t)
		if _, err := Install(context.Background(), f.requestFor(nil)); err != nil {
			t.Fatalf("Install: %v", err)
		}
		if len(f.reread_) != 1 || f.reread_[0] != f.emmec {
			t.Fatalf("re-read = %v, want one call for %s", f.reread_, f.emmec)
		}
	})
	t.Run("a failed re-read keeps the verified clone", func(t *testing.T) {
		f := newFixture(t)
		f.rereadErr = errors.New("partx: not a block device")
		rep, err := Install(context.Background(), f.request())
		if err != nil {
			t.Fatalf("Install after a failed re-read: %v", err)
		}
		if !rep.Written {
			t.Fatal("the clone is verified but the report says it was not written")
		}
		if len(f.reread_) != 1 {
			t.Fatalf("re-read = %v, want one attempt", f.reread_)
		}
	})
}

// TestStagedTempNameIsFATSafe pins the bug the board found: the staging runs
// on the boot partition, which is VFAT, where a colon is not a legal file name
// character, so `.staging:config.json` failed with EINVAL and no file was ever
// staged. A temp name must survive a FAT filesystem.
func TestStagedTempNameIsFATSafe(t *testing.T) {
	dir := t.TempDir()
	for _, name := range MigrateFiles {
		if err := os.WriteFile(filepath.Join(dir, name), []byte("x"), 0o644); err != nil {
			t.Fatal(err)
		}
	}
	out := filepath.Join(t.TempDir(), MigrateDir)
	st, err := stageFiles(dir, out, MigrateFiles, 1<<20, 4096, nil)
	if err != nil {
		t.Fatalf("stageFiles: %v", err)
	}
	if len(st.Files) != len(MigrateFiles) {
		t.Fatalf("staged %v, want %v", st.Files, MigrateFiles)
	}
	// What the boot partition must accept: no colon, no backslash, nothing
	// outside the printable VFAT set, and no leftovers with the temp prefix.
	entries, err := os.ReadDir(out)
	if err != nil {
		t.Fatal(err)
	}
	for _, e := range entries {
		for _, r := range e.Name() {
			if r == ':' || r == '\\' || r == '*' || r == '?' || r == '"' || r == '<' || r == '>' || r == '|' || r < 0x20 {
				t.Fatalf("%q is not a legal VFAT file name", e.Name())
			}
		}
		if strings.HasPrefix(e.Name(), migrateTempPrefix) {
			t.Fatalf("temp file %q left behind", e.Name())
		}
	}
}

// TestInstallDetachesTheStagingMount pins the second bug the board found: the
// cleanup only remounted read-only (with the arguments in the wrong order, so
// it failed), which left the target's boot partition mounted rw — the next
// install then refused because something was mounted from the destination.
func TestInstallDetachesTheStagingMount(t *testing.T) {
	f := newFixture(t)
	if _, err := Install(context.Background(), f.request()); err != nil {
		t.Fatalf("Install: %v", err)
	}
	if len(f.umounts) != 1 {
		t.Fatalf("umounts = %v, want the staging mount point detached once", f.umounts)
	}
	// The read-only remount must come first, in the documented form
	// (dir, "", "remount,ro"), and the mount point must be gone afterwards.
	last := ""
	for _, m := range f.mounts_ {
		last = m
	}
	if !strings.Contains(last, "remount,ro") {
		t.Fatalf("the last mount call was %q, want a remount read-only before the detach", last)
	}
	if _, err := os.Lstat(filepath.Join(f.dir, "run", "wat-stage")); err == nil {
		t.Fatal("the staging mount point was left behind")
	}
}

// TestInstallRepointsTheBootFile pins the bug that made the internal storage
// look dead: the clone copies the card's boot file verbatim, and it names the
// root by device path (/dev/mmcblk0p2). Booted from the eMMC that partition is
// /dev/mmcblk1p2, so the kernel found no root, panicked before userspace, and
// the board had no network and no USB console.
func TestInstallRepointsTheBootFile(t *testing.T) {
	t.Run("the clone names the target disk afterwards", func(t *testing.T) {
		f := newFixture(t)
		rep, err := Install(context.Background(), f.request())
		if err != nil {
			t.Fatalf("Install: %v", err)
		}
		// The fixture's devices are under a temp dir, so the token carries
		// that path: what matters is the disk the root names.
		want := "root=" + f.emmec + "p2"
		if rep.Repointed != want {
			t.Fatalf("repointed = %q, want %q", rep.Repointed, want)
		}
		got := string(read(t, filepath.Join(f.tboot, bootFileRel)))
		if !strings.Contains(got, want) || strings.Contains(got, "mmcblk0p2") {
			t.Fatalf("boot file after the install:\n%s", got)
		}
		// Everything else stays byte-identical.
		if !strings.Contains(got, "label wa") || !strings.Contains(got, "kernel /zImage") {
			t.Fatalf("the boot file lost its other lines:\n%s", got)
		}
	})
	t.Run("a boot file already naming the target is left alone", func(t *testing.T) {
		dir := t.TempDir()
		path := filepath.Join(dir, bootFileRel)
		mustErr(os.MkdirAll(filepath.Dir(path), 0o755))
		before := "append root=/dev/mmcblk1p2 ro\n"
		mustErr(os.WriteFile(path, []byte(before), 0o644))
		token, err := repointBoot(dir, bootFileRel, "/dev/mmcblk0", "/dev/mmcblk1")
		if err != nil || token != "" {
			t.Fatalf("token = %q, err = %v, want no change", token, err)
		}
		if got := string(read(t, path)); got != before {
			t.Fatalf("the file was rewritten: %q", got)
		}
	})
	t.Run("a missing boot file is an error, not silence", func(t *testing.T) {
		if _, err := repointBoot(t.TempDir(), bootFileRel, "/dev/mmcblk0", "/dev/mmcblk1"); err == nil {
			t.Fatal("repointBoot accepted a missing boot file")
		}
	})
}
