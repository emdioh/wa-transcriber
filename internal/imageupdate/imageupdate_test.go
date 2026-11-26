package imageupdate

import (
	"bytes"
	"context"
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"io"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"testing"
	"time"
)

// fakeDevice is the A/B layout made of files: dev/mmcblk0p2 and p3 are the
// "partitions", boot/ holds the boot file, data/ is /data and sys/ has the
// sysfs per-partition directories with the same device number as the
// partition standing in for the running root.
type fakeDevice struct {
	dir    string
	layout Layout
	root   string // the partition file FindSlots sees mounted at Root
}

// newFakeDevice builds the layout and points Root at the second partition
// (B running), the situation after an update to A.
func newFakeDevice(t *testing.T, roots ...string) *fakeDevice {
	t.Helper()
	dir := t.TempDir()
	dev := &fakeDevice{dir: dir, root: "mmcblk0p2"}
	if len(roots) > 0 && roots[0] != "" {
		dev.root = roots[0]
	}
	bootDir := filepath.Join(dir, "boot")
	if err := os.MkdirAll(filepath.Join(bootDir, "extlinux"), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.MkdirAll(filepath.Join(dir, "data"), 0o700); err != nil {
		t.Fatal(err)
	}
	for _, name := range []string{"mmcblk0p2", "mmcblk0p3"} {
		p := filepath.Join(dir, "dev", name)
		if err := os.MkdirAll(filepath.Dir(p), 0o755); err != nil {
			t.Fatal(err)
		}
		f, err := os.Create(p)
		if err != nil {
			t.Fatal(err)
		}
		f.Close()
	}
	// The boot file names a partition the way a board does (/dev/mmcblk0p2),
	// not the way this fake filesystem stores it: the bootloader only ever
	// sees device nodes, so the token is compared on its base name.
	dev.layout = Layout{
		Root:     filepath.Join(dir, "dev", dev.root),
		BootDir:  bootDir,
		BootFile: filepath.Join(bootDir, "extlinux", "extlinux.conf"),
		DataDir:  filepath.Join(dir, "data"),
		SysBlock: filepath.Join(dir, "sys"),
		DevDir:   filepath.Join(dir, "dev") + "/",
		Slots:    [2]string{"p2", "p3"},
	}
	dev.writeSys()
	dev.writeBootFile("mmcblk0p2")
	return dev
}

// writeSys fakes the sysfs entries: both partitions on the same device as the
// partition playing the mounted root.
func (d *fakeDevice) writeSys() {
	_ = os.RemoveAll(filepath.Join(d.dir, "sys"))
	for _, name := range []string{"mmcblk0p2", "mmcblk0p3"} {
		dir := filepath.Join(d.dir, "sys", name)
		if err := os.MkdirAll(dir, 0o755); err != nil {
			panic(err)
		}
		major, minor := 179, 2
		if name == "mmcblk0p3" {
			minor = 3
		}
		if name == d.root {
			fi, err := os.Stat(d.layout.Root)
			if err == nil {
				dev, _ := fsDevice(fi)
				major = int(dev>>8) & 0xfff
				minor = int((dev>>12)<<8 | dev&0xff)
			}
		}
		if err := os.WriteFile(filepath.Join(dir, "dev"),
			[]byte(strconv.Itoa(major)+":"+strconv.Itoa(minor)+"\n"), 0o644); err != nil {
			panic(err)
		}
	}
}

// bootConf is a plausible extlinux.conf: the update has to keep every one of
// these bytes except the root token.
const bootConf = `DEFAULT wat
LABEL wat
  LINUX /zImage
  FDT /am335x-boneblack.dtb
  APPEND root=/dev/mmcblk0p2 ro rootwait rootfstype=ext4 console=ttyO0,115200n8
`

func (d *fakeDevice) writeBootFile(part string) {
	body := strings.ReplaceAll(bootConf, "/dev/mmcblk0p2", "/dev/"+part)
	if err := os.WriteFile(d.layout.BootFile, []byte(body), 0o644); err != nil {
		panic(err)
	}
}

func (d *fakeDevice) bootFile() string {
	b, err := os.ReadFile(d.layout.BootFile)
	if err != nil {
		return ""
	}
	return string(b)
}

func (d *fakeDevice) part(name string) string { return filepath.Join(d.dir, "dev", name) }

func (d *fakeDevice) service() *Service { return &Service{Layout: d.layout} }

// tampered returns a copy of b with one byte changed.
func tampered(t *testing.T, b []byte) string {
	t.Helper()
	c := append([]byte(nil), b...)
	c[len(c)/2] ^= 0xff
	p := filepath.Join(t.TempDir(), "tampered")
	if err := os.WriteFile(p, c, 0o644); err != nil {
		t.Fatal(err)
	}
	return p
}

// image builds a test image of n bytes with a known hash.
func image(n int) ([]byte, string) {
	b := make([]byte, n)
	for i := range b {
		b[i] = byte('a' + i%26)
	}
	sum := sha256.Sum256(b)
	return b, hex.EncodeToString(sum[:])
}

func TestFindSlotsMmcblk(t *testing.T) {
	d := newFakeDevice(t)
	slots, err := d.layout.FindSlots()
	if err != nil {
		t.Fatalf("FindSlots: %v", err)
	}
	if slots.Disk != "mmcblk0" || slots.Current.Path != d.part("mmcblk0p2") || slots.Other.Path != d.part("mmcblk0p3") {
		t.Fatalf("slots: %+v", slots)
	}
	if slots.Current.Side != "A" || slots.Other.Side != "B" {
		t.Fatalf("sides: %+v", slots)
	}
}

func TestParentDiskNaming(t *testing.T) {
	for _, tc := range []struct {
		running, disk string
		slots         [2]string
	}{
		{"mmcblk0p2", "mmcblk0", [2]string{"p2", "p3"}},
		{"mmcblk1p3", "mmcblk1", [2]string{"p2", "p3"}},
		{"sda2", "sda", [2]string{"2", "3"}},
		{"vda3", "vda", [2]string{"2", "3"}},
		{"nvme0n1p2", "nvme0n1", [2]string{"p2", "p3"}},
	} {
		got, ok := parentDisk(tc.running, tc.slots)
		if !ok || got != tc.disk {
			t.Fatalf("%s: parent %q, %v; want %s", tc.running, got, ok, tc.disk)
		}
	}
	if _, ok := parentDisk("sdb1", [2]string{"p2", "p3"}); ok {
		t.Fatal("a partition outside the layout was accepted")
	}
}

// TestFindSlotsRefuses covers the cases where the two slots cannot be told
// apart: the device must refuse rather than guess.
func TestFindSlotsRefuses(t *testing.T) {
	cases := map[string]func(d *fakeDevice){
		"root is not a slot": func(d *fakeDevice) {
			// sd card in a reader next to the real root: a different name.
			d.root = "mmcblk0p2"
			d.writeSys()
			os.Rename(filepath.Join(d.dir, "sys", "mmcblk0p2"), filepath.Join(d.dir, "sys", "sda1"))
		},
		"no sysfs entry": func(d *fakeDevice) {
			os.RemoveAll(filepath.Join(d.dir, "sys", "mmcblk0p2"))
		},
		"two entries for one device": func(d *fakeDevice) {
			os.MkdirAll(filepath.Join(d.dir, "sys", "mmcblk0"), 0o755)
			b, _ := os.ReadFile(filepath.Join(d.dir, "sys", "mmcblk0p2", "dev"))
			os.WriteFile(filepath.Join(d.dir, "sys", "mmcblk0", "dev"), b, 0o644)
		},
		"root missing": func(d *fakeDevice) {
			os.Remove(d.layout.Root)
		},
	}
	for name, mutate := range cases {
		t.Run(name, func(t *testing.T) {
			d := newFakeDevice(t)
			mutate(d)
			if _, err := d.layout.FindSlots(); !errors.Is(err, ErrNoSlots) {
				t.Fatalf("FindSlots: %v, want ErrNoSlots", err)
			}
		})
	}
}

func TestUpdateWritesInactiveSlot(t *testing.T) {
	ctx := context.Background()

	t.Run("happy path", func(t *testing.T) {
		d := newFakeDevice(t) // A running
		body, sum := image(4096)
		svc := d.service()
		slot, err := svc.Install(ctx, bytes.NewReader(body), int64(len(body)), sum)
		if err != nil {
			t.Fatalf("Install: %v", err)
		}
		if slot.Path != d.part("mmcblk0p3") || slot.Side != "B" {
			t.Fatalf("wrote %+v, want the inactive p3", slot)
		}
		written, err := os.ReadFile(d.part("mmcblk0p3"))
		if err != nil {
			t.Fatal(err)
		}
		if !bytes.Equal(written, body) {
			t.Fatal("the inactive slot does not hold the image")
		}
		if fi, err := os.Stat(d.part("mmcblk0p2")); err != nil || fi.Size() != 0 {
			t.Fatalf("the running slot was touched: %v %v", fi, err)
		}
		if !strings.Contains(d.bootFile(), "root=/dev/mmcblk0p2") {
			t.Fatal("install changed the boot file")
		}
	})

	t.Run("running slot is B", func(t *testing.T) {
		d := newFakeDevice(t, "mmcblk0p3")
		d.writeBootFile("mmcblk0p3")
		body, sum := image(256)
		slot, err := d.service().Install(ctx, bytes.NewReader(body), int64(len(body)), sum)
		if err != nil {
			t.Fatalf("Install: %v", err)
		}
		if slot.Path != d.part("mmcblk0p2") || slot.Side != "A" {
			t.Fatalf("wrote %+v, want the inactive p2", slot)
		}
	})

	t.Run("truncated source", func(t *testing.T) {
		d := newFakeDevice(t)
		body, sum := image(4096)
		if _, err := d.service().Install(ctx, bytes.NewReader(body[:1000]), int64(len(body)), sum); err == nil {
			t.Fatal("a truncated source was accepted")
		}
		if fi, _ := os.Stat(d.part("mmcblk0p3")); fi.Size() != 1000 {
			t.Fatalf("wrote %d bytes, expected the 1000 the source had", fi.Size())
		}
		if strings.Contains(d.bootFile(), "p3") {
			t.Fatal("a failed install switched the boot file")
		}
	})

	t.Run("sha256 mismatch", func(t *testing.T) {
		d := newFakeDevice(t)
		body, _ := image(4096)
		other, err := HashDevice(tampered(t, body))
		if err != nil {
			t.Fatal(err)
		}
		// Same size, different bytes: only the hash can tell them apart.
		_, err = d.service().Install(ctx, bytes.NewReader(body), int64(len(body)), other)
		if err == nil || !strings.Contains(err.Error(), "sha256") {
			t.Fatalf("Install: %v, want a sha256 error", err)
		}
	})

	t.Run("read-back mismatch", func(t *testing.T) {
		d := newFakeDevice(t)
		body, sum := image(4096)
		svc := d.service()
		svc.VerifyRead = func(string) (io.Reader, error) {
			// A flash that reads back different bytes.
			return bytes.NewReader([]byte("not what we wrote")), nil
		}
		_, err := svc.Install(ctx, bytes.NewReader(body), int64(len(body)), sum)
		if err == nil || !strings.Contains(err.Error(), "reads back") {
			t.Fatalf("Install: %v, want a read-back error", err)
		}
	})

	t.Run("slot too small", func(t *testing.T) {
		d := newFakeDevice(t)
		body, sum := image(4096)
		if err := os.Truncate(d.part("mmcblk0p3"), 128); err != nil {
			t.Fatal(err)
		}
		if _, err := d.service().Install(ctx, bytes.NewReader(body), int64(len(body)), sum); err == nil {
			t.Fatal("an image larger than the slot was accepted")
		}
	})

	t.Run("ambiguous slots", func(t *testing.T) {
		d := newFakeDevice(t)
		os.RemoveAll(filepath.Join(d.dir, "sys"))
		body, sum := image(64)
		if _, err := d.service().Install(ctx, bytes.NewReader(body), int64(len(body)), sum); !errors.Is(err, ErrNoSlots) {
			t.Fatalf("Install: %v, want ErrNoSlots", err)
		}
		if fi, _ := os.Stat(d.part("mmcblk0p3")); fi.Size() != 0 {
			t.Fatal("a refused install still wrote a partition")
		}
	})

	t.Run("cancelled download", func(t *testing.T) {
		d := newFakeDevice(t)
		body, sum := image(4096)
		ctx, cancel := context.WithCancel(ctx)
		cancel()
		if _, err := d.service().Install(ctx, bytes.NewReader(body), int64(len(body)), sum); err == nil {
			t.Fatal("a cancelled install was accepted")
		}
	})
}

func TestReplaceRootKeepsEveryOtherByte(t *testing.T) {
	in := []byte("APPEND root=/dev/mmcblk0p2 ro rootwait\r\nFDT /x.dtb")
	out, err := replaceRoot(in, "/dev/mmcblk0p2", "/dev/mmcblk0p3")
	if err != nil {
		t.Fatalf("replaceRoot: %v", err)
	}
	want := string(bytes.Replace(in, []byte("root=/dev/mmcblk0p2"), []byte("root=/dev/mmcblk0p3"), 1))
	if string(out) != want {
		t.Fatalf("got %q, want %q", out, want)
	}
	if len(out) != len(in) {
		t.Fatalf("length changed: %d → %d", len(in), len(out))
	}
	// A prefix of the right token must not match.
	if _, err := replaceRoot([]byte("root=/dev/mmcblk0p233 "), "/dev/mmcblk0p2", "/dev/mmcblk0p3"); err == nil {
		t.Fatal("root=/dev/mmcblk0p233 matched root=/dev/mmcblk0p2")
	}
	if _, err := replaceRoot([]byte("root=/dev/mmcblk0p2 root=/dev/mmcblk0p2"), "/dev/mmcblk0p2", "/dev/mmcblk0p3"); err == nil {
		t.Fatal("a boot file naming the root twice was rewritten")
	}
	if _, err := replaceRoot([]byte("ro quiet"), "/dev/mmcblk0p2", "/dev/mmcblk0p3"); err == nil {
		t.Fatal("a boot file without the root token was rewritten")
	}
}

func TestSwitchRewritesOnlyTheToken(t *testing.T) {
	d := newFakeDevice(t)
	body, sum := image(1024)
	if _, err := d.service().Install(context.Background(), bytes.NewReader(body), int64(len(body)), sum); err != nil {
		t.Fatal(err)
	}
	before := d.bootFile()
	if err := d.service().Switch("v1.2.0"); err != nil {
		t.Fatalf("Switch: %v", err)
	}
	after := d.bootFile()
	if !strings.Contains(after, "root=/dev/mmcblk0p3 ") {
		t.Fatalf("boot file after the switch: %q", after)
	}
	if len(after) != len(before) {
		t.Fatalf("the file length changed: %d → %d", len(before), len(after))
	}
	want := strings.Replace(before, "root=/dev/mmcblk0p2", "root=/dev/mmcblk0p3", 1)
	if after != want {
		t.Fatalf("boot file changed beyond the token:\n got %q\nwant %q", after, want)
	}
	// The temp file of the atomic write is gone.
	entries, _ := os.ReadDir(filepath.Dir(d.layout.BootFile))
	for _, e := range entries {
		if strings.HasPrefix(e.Name(), ".") {
			t.Fatalf("a temp file was left behind: %s", e.Name())
		}
	}
	st, err := LoadBoot(d.layout.DataDir)
	if err != nil || st.Pending == nil || st.Pending.Side != "B" || st.Pending.Image != "v1.2.0" {
		t.Fatalf("boot state after the switch: %+v %v", st, err)
	}
	if st.Previous == nil || st.Previous.Side != "A" {
		t.Fatalf("previous slot not recorded: %+v", st.Previous)
	}
	if st.Pending.SHA256 != sum {
		t.Fatalf("pending hash %q, want %q", st.Pending.SHA256, sum)
	}
}

func TestSwitchFailsWithReadOnlyBoot(t *testing.T) {
	d := newFakeDevice(t)
	body, sum := image(512)
	if _, err := d.service().Install(context.Background(), bytes.NewReader(body), int64(len(body)), sum); err != nil {
		t.Fatal(err)
	}
	before := d.bootFile()
	svc := d.service()
	svc.BootRW = func() (func() error, error) { return nil, errors.New("mount -o remount,rw /boot: EROFS") }
	if err := svc.Switch("v1"); err == nil {
		t.Fatal("a switch without a writable boot partition succeeded")
	}
	if d.bootFile() != before {
		t.Fatal("a failed switch changed the boot file")
	}
	st, err := LoadBoot(d.layout.DataDir)
	if err != nil && st == nil {
		t.Fatal(err)
	}
	if st == nil {
		t.Fatal("the boot state was dropped by a failed switch")
	}
	// The verified write is still recorded (the slot is installable again),
	// but nobody claims the boot file points there.
	if st.Previous != nil {
		t.Fatalf("a failed switch recorded a previous slot: %+v", st.Previous)
	}
	if st.Pending == nil || st.Pending.Image != "" {
		t.Fatalf("a failed switch changed the verified slot: %+v", st.Pending)
	}
}

func TestSwitchRefusesDamagedSlot(t *testing.T) {
	d := newFakeDevice(t)
	body, sum := image(512)
	if _, err := d.service().Install(context.Background(), bytes.NewReader(body), int64(len(body)), sum); err != nil {
		t.Fatal(err)
	}
	// Something overwrote the partition after the verified write.
	if err := os.WriteFile(d.part("mmcblk0p3"), []byte("garbage"), 0o644); err != nil {
		t.Fatal(err)
	}
	if err := d.service().Switch("v1"); err == nil {
		t.Fatal("Switch accepted a slot that no longer matches its hash")
	}
	if !strings.Contains(d.bootFile(), "root=/dev/mmcblk0p2") {
		t.Fatal("the boot file moved to a damaged slot")
	}
}

// bootScenario runs Boot three times, the way the device would.
func bootScenario(t *testing.T, d *fakeDevice, n int) (BootAction, error) {
	t.Helper()
	var act BootAction
	var err error
	for i := 0; i < n; i++ {
		act, err = Boot(d.layout, time.Now())
	}
	return act, err
}

func TestBootCountsAttemptsAndRollsBack(t *testing.T) {
	d := newFakeDevice(t) // A running
	body, sum := image(256)
	if _, err := d.service().Install(context.Background(), bytes.NewReader(body), int64(len(body)), sum); err != nil {
		t.Fatal(err)
	}
	if err := d.service().Switch("v1"); err != nil {
		t.Fatal(err)
	}
	// The device rebooted into B: / is now p3.
	d.root = "mmcblk0p3"
	d.layout.Root = d.part("mmcblk0p3")
	d.writeSys()

	for i := 1; i < MaxBootTries; i++ {
		act, err := Boot(d.layout, time.Now())
		if err != nil {
			t.Fatalf("boot %d: %v", i, err)
		}
		if act.Reboot {
			t.Fatalf("boot %d asked for a reboot too early", i)
		}
		if !strings.Contains(act.Note, "waiting for the health gate") {
			t.Fatalf("boot %d note: %q", i, act.Note)
		}
	}
	act, err := Boot(d.layout, time.Now())
	if err != nil {
		t.Fatalf("third boot: %v", err)
	}
	if !act.Reboot {
		t.Fatalf("the third unconfirmed start did not fall back: %+v", act)
	}
	if !strings.Contains(d.bootFile(), "root=/dev/mmcblk0p2 ") {
		t.Fatalf("the boot file was not moved back: %q", d.bootFile())
	}
	st, err := LoadBoot(d.layout.DataDir)
	if err != nil {
		t.Fatal(err)
	}
	if st.Pending != nil {
		t.Fatalf("pending slot left after a fallback: %+v", st.Pending)
	}
	if st.Failure == nil || st.Failure.Side != "B" {
		t.Fatalf("the fallback was not recorded: %+v", st.Failure)
	}
}

func TestConfirmClearsTheGate(t *testing.T) {
	d := newFakeDevice(t)
	body, sum := image(256)
	svc := d.service()
	if _, err := svc.Install(context.Background(), bytes.NewReader(body), int64(len(body)), sum); err != nil {
		t.Fatal(err)
	}
	if err := svc.Switch("v1"); err != nil {
		t.Fatal(err)
	}
	d.root = "mmcblk0p3"
	d.layout.Root = d.part("mmcblk0p3")
	d.writeSys()

	if _, err := Boot(d.layout, time.Now()); err != nil {
		t.Fatal(err)
	}
	// Confirm hashes the running partition: make it hold what Switch saw.
	if err := os.WriteFile(d.part("mmcblk0p3"), body, 0o644); err != nil {
		t.Fatal(err)
	}
	if err := d.service().Confirm(); err != nil {
		t.Fatalf("Confirm: %v", err)
	}
	st, err := LoadBoot(d.layout.DataDir)
	if err != nil {
		t.Fatal(err)
	}
	if st.Pending == nil || !st.Pending.Confirmed || st.Pending.Tries != 0 {
		t.Fatalf("state after the confirmation: %+v", st.Pending)
	}
	// A confirmed slot is never rolled back, however often it starts.
	for i := 0; i < MaxBootTries+2; i++ {
		act, err := Boot(d.layout, time.Now())
		if err != nil || act.Reboot {
			t.Fatalf("boot %d after the confirmation: %+v %v", i, act, err)
		}
	}
	if !strings.Contains(d.bootFile(), "root=/dev/mmcblk0p3 ") {
		t.Fatal("a confirmed slot was rolled back")
	}
}

func TestConfirmRefusesSwappedSlot(t *testing.T) {
	d := newFakeDevice(t)
	body, sum := image(256)
	svc := d.service()
	if _, err := svc.Install(context.Background(), bytes.NewReader(body), int64(len(body)), sum); err != nil {
		t.Fatal(err)
	}
	if err := svc.Switch("v1"); err != nil {
		t.Fatal(err)
	}
	d.root = "mmcblk0p3"
	d.layout.Root = d.part("mmcblk0p3")
	d.writeSys()
	// The partition under the kernel is not what was verified.
	if err := os.WriteFile(d.part("mmcblk0p3"), []byte("other"), 0o644); err != nil {
		t.Fatal(err)
	}
	if err := d.service().Confirm(); err == nil {
		t.Fatal("Confirm accepted a slot that does not match its hash")
	}
}

func TestRollbackGoesBack(t *testing.T) {
	d := newFakeDevice(t)
	body, sum := image(256)
	svc := d.service()
	if _, err := svc.Install(context.Background(), bytes.NewReader(body), int64(len(body)), sum); err != nil {
		t.Fatal(err)
	}
	st := svc.Status()
	if st.CanRollback {
		t.Fatal("a rollback is offered before an image was installed")
	}
	if err := svc.Switch("v1"); err != nil {
		t.Fatal(err)
	}
	d.root = "mmcblk0p3"
	d.layout.Root = d.part("mmcblk0p3")
	d.writeSys()
	st = svc.Status()
	if !st.Ready || st.Slot != "B" || !st.CanRollback || st.RollbackTo != "A" {
		t.Fatalf("status on B after an update: %+v", st)
	}
	if err := svc.Rollback(); err != nil {
		t.Fatalf("Rollback: %v", err)
	}
	if !strings.Contains(d.bootFile(), "root=/dev/mmcblk0p2 ") {
		t.Fatalf("boot file after the rollback: %q", d.bootFile())
	}
	// Both directions keep working.
	bs, err := LoadBoot(d.layout.DataDir)
	if err != nil {
		t.Fatal(err)
	}
	if bs.Previous == nil || bs.Previous.Side != "B" {
		t.Fatalf("the slot left by the rollback is not the new previous: %+v", bs.Previous)
	}
}

func TestStatusOnOldLayout(t *testing.T) {
	d := newFakeDevice(t)
	os.RemoveAll(filepath.Join(d.dir, "sys"))
	st := d.service().Status()
	if st.Ready {
		t.Fatalf("an old layout reports ready: %+v", st)
	}
}

func TestCmdlineSwitchPreservesInit(t *testing.T) {
	const rpiCmdline = "root=/dev/mmcblk0p2 rootfstype=ext4 ro rootwait console=tty1 console=ttyAMA0,115200 init=/usr/bin/wa-transcriber wat.network=none wat.listen=:8080\n"

	// Direct replaceRoot round-trip: p2 -> p3 -> p2 keeps init=, wat.* flags,
	// and the single-line layout required by Raspberry Pi firmware.
	toB, err := replaceRoot([]byte(rpiCmdline), "/dev/mmcblk0p2", "/dev/mmcblk0p3")
	if err != nil {
		t.Fatalf("replaceRoot p2->p3: %v", err)
	}
	wantB := strings.Replace(rpiCmdline, "root=/dev/mmcblk0p2", "root=/dev/mmcblk0p3", 1)
	if string(toB) != wantB {
		t.Fatalf("p2->p3:\n got %q\nwant %q", toB, wantB)
	}
	if strings.Count(string(toB), "\n") != 1 {
		t.Fatalf("cmdline.txt stopped being a single line: %q", toB)
	}
	backA, err := replaceRoot(toB, "/dev/mmcblk0p3", "/dev/mmcblk0p2")
	if err != nil {
		t.Fatalf("replaceRoot p3->p2: %v", err)
	}
	if string(backA) != rpiCmdline {
		t.Fatalf("p3->p2 round-trip drifted:\n got %q\nwant %q", backA, rpiCmdline)
	}

	// End-to-end Switch and Rollback against a Pi cmdline.txt boot file.
	d := newFakeDevice(t)
	d.layout.BootFile = filepath.Join(d.layout.BootDir, "cmdline.txt")
	if err := os.WriteFile(d.layout.BootFile, []byte(rpiCmdline), 0o644); err != nil {
		t.Fatal(err)
	}
	body, sum := image(1024)
	if _, err := d.service().Install(context.Background(), bytes.NewReader(body), int64(len(body)), sum); err != nil {
		t.Fatalf("Install: %v", err)
	}
	if err := d.service().Switch("v1.3.0"); err != nil {
		t.Fatalf("Switch: %v", err)
	}
	if got := d.bootFile(); got != wantB {
		t.Fatalf("cmdline.txt after Switch:\n got %q\nwant %q", got, wantB)
	}
	if got := bootRoot(d.layout.BootFile); got != "/dev/mmcblk0p3" {
		t.Fatalf("bootRoot after Switch = %q, want /dev/mmcblk0p3", got)
	}

	// Boot into B and roll back to A.
	d.root = "mmcblk0p3"
	d.layout.Root = d.part("mmcblk0p3")
	d.writeSys()
	if err := d.service().Rollback(); err != nil {
		t.Fatalf("Rollback: %v", err)
	}
	if got := d.bootFile(); got != rpiCmdline {
		t.Fatalf("cmdline.txt after Rollback:\n got %q\nwant %q", got, rpiCmdline)
	}
}
