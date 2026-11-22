package flash

import (
	"context"
	"errors"
	"fmt"
	"io"
	"os"
	"path/filepath"
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
	card    string // /dev-like path of the running card
	emmec   string // ... of the internal storage
	mounts  string
	source  string // the card's image bytes
	regions []Region
}

// newFixture builds a card with a 4 KiB MBR region and three 8 KiB
// partitions, a 4 MiB eMMC disk and the sysfs entries that describe both.
func newFixture(t *testing.T) *fixture {
	t.Helper()
	dir := t.TempDir()
	f := &fixture{
		dir:  dir,
		sys:  filepath.Join(dir, "sys"),
		dev:  filepath.Join(dir, "dev") + "/",
		boot: filepath.Join(dir, "boot"),
	}
	f.mounts = filepath.Join(dir, "mounts")
	if err := os.MkdirAll(f.dev, 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.MkdirAll(f.boot, 0o755); err != nil {
		t.Fatal(err)
	}
	// The running card: the SD slot, removable, 64 KiB of image.
	f.card = f.dev + "mmcblk0"
	f.mkdisk("mmcblk0", "MMC", true, 64<<10)
	f.source = f.writeDisk(f.card, 64<<10, 0x5a)
	f.regions = Regions(512, 8<<10, 8<<10, 8<<10)
	// The eMMC: not removable, 4 MiB and empty.
	f.emmec = f.dev + "mmcblk1"
	f.mkdisk("mmcblk1", "MMC", false, 4<<20)
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
		Mounts:     f.mounts,
	}
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
	var called int
	req := f.request()
	req.WithBootRW = func() (func() error, error) {
		called++
		return func() error { called++; return nil }, nil
	}
	rep, err := Install(context.Background(), req)
	if err != nil {
		t.Fatal(err)
	}
	if !rep.Written || rep.Plan.Target.Name != "mmcblk1" {
		t.Fatalf("report = %+v", rep)
	}
	if called != 2 {
		t.Fatalf("WithBootRW called %d times, want enter and leave", called)
	}
	m, err := ReadMarker(f.boot)
	if err != nil {
		t.Fatal(err)
	}
	if m == nil || m.From != "/dev/mmcblk0p4" || len(m.Files) != len(MigrateFiles) {
		t.Fatalf("marker = %+v", m)
	}
	if !strings.Contains(string(read(t, filepath.Join(f.boot, MarkerName))), "file session.db-wal") {
		t.Fatal("the marker does not list session.db-wal")
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
			req.WithBootRW = func() (func() error, error) {
				t.Fatal("the boot partition was made writable for a refused install")
				return nil, nil
			}
			tc.modify(&req)
			_, err := Install(context.Background(), req)
			if !errors.Is(err, tc.want) {
				t.Fatalf("err = %v, want %v", err, tc.want)
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
	if _, err := parseMarker("wat-migrate 1\nfile config.json\n"); err == nil {
		t.Fatal("a marker without a source partition was accepted")
	}
	if got := NewMarker("sda").From; got != "/dev/sda4" {
		t.Fatalf("devPart of sda4 = %q", got)
	}
}
