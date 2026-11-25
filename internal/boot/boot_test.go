package boot

import (
	"context"
	"fmt"
	"log/slog"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"golang.org/x/sys/unix"
)

// fakeMachine is a board made of files: a mount table that grows when
// something is mounted, a /dev with p1..p4, and a /sys/dev/block symlink that
// names the root partition the way the running kernel does.
type fakeMachine struct {
	t         *testing.T
	dir       string
	devDir    string
	sysDir    string
	mounts    string
	mountFail map[string]int // target -> how many mounts to refuse
	calls     []mountSpec
	runs      []string
	runFunc   func(stdin, name string, args []string) error
	ifUp      []string
}

func newFakeMachine(t *testing.T) *fakeMachine {
	t.Helper()
	dir := t.TempDir()
	m := &fakeMachine{
		t:         t,
		dir:       dir,
		devDir:    filepath.Join(dir, "dev"),
		sysDir:    filepath.Join(dir, "sys"),
		mounts:    filepath.Join(dir, "mountinfo"),
		mountFail: map[string]int{},
	}
	for _, d := range []string{m.devDir, filepath.Join(m.sysDir, "devices/platform/soc/mmcblk0"), filepath.Join(m.sysDir, "dev/block")} {
		if err := os.MkdirAll(d, 0o755); err != nil {
			t.Fatal(err)
		}
	}
	// /dev/mmcblk0p2 is the running root, exactly as on a card.
	for _, p := range []string{"p1", "p2", "p3"} {
		writeFile(t, filepath.Join(m.devDir, "mmcblk0"+p), nil)
	}
	part := filepath.Join(m.sysDir, "devices/platform/soc/mmcblk0/mmcblk0p2")
	writeFile(t, part, nil)
	if err := os.Symlink("../../devices/platform/soc/mmcblk0/mmcblk0p2", filepath.Join(m.sysDir, "dev/block/179:2")); err != nil {
		t.Fatal(err)
	}
	writeFile(t, m.mounts, []byte(
		"36 35 179:2 / / rw - ext4 /dev/root rw\n"+
			"10 1 0:5 / /proc rw - proc proc rw\n"))
	return m
}

func writeFile(t *testing.T, path string, b []byte) {
	t.Helper()
	if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(path, b, 0o600); err != nil {
		t.Fatal(err)
	}
}

func (m *fakeMachine) addPartition(n int) string {
	p := filepath.Join(m.devDir, fmt.Sprintf("mmcblk0p%d", n))
	writeFile(m.t, p, nil)
	return p
}

func (m *fakeMachine) mount(src, target, fstype string, flags uintptr, data string) error {
	if n := m.mountFail[target]; n > 0 {
		m.mountFail[target] = n - 1
		return unix.EIO
	}
	m.calls = append(m.calls, mountSpec{source: src, target: target, fstype: fstype, flags: flags, data: data})
	line := fmt.Sprintf("90 1 0:99 / %s rw - %s %s rw\n", target, fstype, src)
	f, err := os.OpenFile(m.mounts, os.O_APPEND|os.O_WRONLY, 0o600)
	if err != nil {
		m.t.Fatal(err)
	}
	defer f.Close()
	_, _ = f.WriteString(line)
	return nil
}

func (m *fakeMachine) options() Options {
	return Options{
		Log:      slog.New(slog.NewTextHandler(&strings.Builder{}, &slog.HandlerOptions{Level: slog.LevelError})),
		Mounts:   m.mounts,
		ProcDir:  filepath.Join(m.dir, "proc"),
		DevDir:   m.devDir + "/",
		SysDir:   m.sysDir,
		Wait:     50 * time.Millisecond,
		Mount:    m.mount,
		ReadFile: os.ReadFile,
		MkdirAll: func(string, os.FileMode) error { return nil },
		IfUp:     func(name string) error { m.ifUp = append(m.ifUp, name); return nil },
		Run: func(_ context.Context, stdin, name string, args ...string) error {
			m.runs = append(m.runs, record(name, args, stdin))
			if m.runFunc != nil {
				return m.runFunc(stdin, name, args)
			}
			return nil
		},
	}
}

func (m *fakeMachine) mounted(target string) *mountSpec {
	for i := range m.calls {
		if m.calls[i].target == target {
			return &m.calls[i]
		}
	}
	return nil
}

func (m *fakeMachine) ran(prefix string) bool {
	for _, r := range m.runs {
		if strings.HasPrefix(r, prefix) {
			return true
		}
	}
	return false
}

func TestMountTable(t *testing.T) {
	p := filepath.Join(t.TempDir(), "mountinfo")
	writeFile(t, p, []byte(
		"36 35 98:0 / /mnt/with\\040space rw,relatime master:1 - ext4 /dev/root rw\n"+
			"23 22 0:21 / /tmp rw,nosuid - tmpfs tmpfs rw,size=64M\n"+
			"garbage\n"))
	tbl, err := mountTable(p)
	if err != nil {
		t.Fatal(err)
	}
	if !tbl["/mnt/with space"] || !tbl["/tmp"] || len(tbl) != 2 {
		t.Fatalf("mountTable = %v", tbl)
	}
}

func TestSetupMountsPseudoFilesystemsOnce(t *testing.T) {
	m := newFakeMachine(t)
	o := m.options()
	// /data already mounted, so Setup only has to do the pseudo filesystems,
	// /boot and the modules.
	writeFile(t, m.mounts, append(mustRead(t, m.mounts), []byte("91 1 179:4 / /data rw - ext4 /dev/mmcblk0p4 rw\n")...))
	writeFile(t, filepath.Join(m.devDir, "mmcblk0p1"), fatImage())
	o.HostnameFile = filepath.Join(m.dir, "hostname")
	writeFile(t, o.HostnameFile, []byte("wa\n"))
	o.Modules = []string{"omap_wdt"}

	if err := Setup(context.Background(), o); err != nil {
		t.Fatalf("Setup: %v", err)
	}
	for _, target := range []string{"/sys", "/dev/pts", "/dev/shm", "/tmp", "/run", "/var/log", "/var/lib", "/var/lib/misc", "/boot"} {
		if m.mounted(target) == nil {
			t.Errorf("%s was not mounted (mounted: %v)", target, targets(m.calls))
		}
	}
	if m.mounted("/proc") != nil {
		t.Error("/proc was mounted again although the table already had it")
	}
	if m.mounted("/data") != nil {
		t.Error("/data was mounted again although the table already had it")
	}
	if len(m.ifUp) != 1 || m.ifUp[0] != "lo" {
		t.Errorf("interfaces brought up = %v, want lo", m.ifUp)
	}
	if !m.ran("modprobe omap_wdt") {
		t.Errorf("the watchdog module was not loaded: %v", m.runs)
	}
	if b := m.mounted("/boot"); b == nil || b.fstype != "vfat" || b.flags&unix.MS_RDONLY == 0 {
		t.Errorf("/boot = %+v, want a read-only vfat mount", b)
	}
	// Idempotency: the second run (a re-exec after an update) mounts nothing
	// new.
	before := len(m.calls)
	if err := Setup(context.Background(), o); err != nil {
		t.Fatalf("second Setup: %v", err)
	}
	if len(m.calls) != before {
		t.Errorf("the second Setup mounted %d more filesystems: %v", len(m.calls)-before, m.calls[before:])
	}
}

// TestEnsureMountResolvesSymlinks: /var/log is a symlink to ../tmp on the
// image, so a naive mount does not land where it says and the "already
// mounted" check never matches it (three stacked tmpfs mounts on a board).
func TestEnsureMountResolvesSymlinks(t *testing.T) {
	m := newFakeMachine(t)
	o := m.options()
	root := t.TempDir()
	for _, d := range []string{"tmp", "var"} {
		if err := os.MkdirAll(filepath.Join(root, d), 0o755); err != nil {
			t.Fatal(err)
		}
	}
	// Exactly the image's shape: /var/log -> ../tmp.
	if err := os.Symlink("../tmp", filepath.Join(root, "var", "log")); err != nil {
		t.Fatal(err)
	}
	// The resolved target is already mounted.
	writeFile(t, m.mounts, append(mustRead(t, m.mounts),
		[]byte("91 1 0:99 / "+filepath.Join(root, "tmp")+" rw - tmpfs tmpfs rw\n")...))

	err := o.ensureMount(mountSpec{source: "tmpfs", target: filepath.Join(root, "var", "log"), fstype: "tmpfs", data: "size=8M"})
	if err != nil {
		t.Fatal(err)
	}
	if len(m.calls) != 0 {
		t.Fatalf("mounts = %+v; a mount point that resolves to an already mounted path was mounted again", m.calls)
	}
}

func TestMountDataKeepsLabeledData(t *testing.T) {
	m := newFakeMachine(t)
	writeFile(t, filepath.Join(m.devDir, "mmcblk0p4"), ext4Image(DataLabel))
	o := m.options()

	if err := m.mountDataOnly(context.Background(), o); err != nil {
		t.Fatalf("mountData: %v", err)
	}
	got := m.mounted("/data")
	if got == nil || got.source != filepath.Join(m.devDir, "mmcblk0p4") || got.fstype != "ext4" {
		t.Fatalf("/data = %+v, want p4 as ext4", got)
	}
	if len(m.runs) != 0 {
		t.Fatalf("a labeled DATA partition must not run anything: %v", m.runs)
	}
}

func TestMountDataCreatesPartition(t *testing.T) {
	m := newFakeMachine(t)
	o := m.options()
	// sfdisk is what makes the node appear in the kernel.
	o.Run = func(_ context.Context, stdin, name string, args ...string) error {
		m.runs = append(m.runs, record(name, args, stdin))
		if name == "sfdisk" {
			writeFile(t, filepath.Join(m.devDir, "mmcblk0p4"), nil)
		}
		return nil
	}
	if err := m.mountDataOnly(context.Background(), o); err != nil {
		t.Fatalf("mountData: %v", err)
	}
	disk := m.devDir + "/mmcblk0"
	if want := record("sfdisk", []string{"--append", "--no-reread", disk}, ",+,83\n"); !contains(m.runs, want) {
		t.Fatalf("sfdisk was not called as expected (%q): %v", want, m.runs)
	}
	if want := record("mkfs.ext4", []string{"-q", "-L", DataLabel, disk + "p4"}, ""); !contains(m.runs, want) {
		t.Fatalf("mkfs.ext4 was not called as expected (%q): %v", want, m.runs)
	}
	if m.mounted("/data") == nil {
		t.Fatal("/data was not mounted")
	}
}

func TestMountDataLeavesForeignFilesystemAlone(t *testing.T) {
	m := newFakeMachine(t)
	writeFile(t, filepath.Join(m.devDir, "mmcblk0p4"), ext4Image("SOMETHING"))
	o := m.options()

	err := m.mountDataOnly(context.Background(), o)
	if err == nil || !strings.Contains(err.Error(), "without the DATA label") {
		t.Fatalf("mountData = %v, want a refusal naming the label", err)
	}
	if m.mounted("/data") != nil {
		t.Fatal("/data was mounted from a foreign filesystem")
	}
	if len(m.runs) != 0 {
		t.Fatalf("a foreign filesystem must not be formatted: %v", m.runs)
	}
}

// TestMountDataNeverUsesTheRootSlots: the label is the identity, but p2 and
// p3 are root slots. A mislabelled root slot (a copied card, an image update
// that wrote a filesystem there) must never become /data — that would write
// the session onto the slot an update is about to overwrite.
func TestMountDataNeverUsesRootSlots(t *testing.T) {
	m := newFakeMachine(t)
	writeFile(t, filepath.Join(m.devDir, "mmcblk0p2"), ext4Image(DataLabel))
	o := m.options()

	// p2 carries DATA and there is no p4: Setup creates p4 rather than
	// adopting the running root slot.
	o.Run = func(_ context.Context, _, name string, args ...string) error {
		m.runs = append(m.runs, strings.TrimSpace(name+" "+strings.Join(args, " ")))
		if name == "sfdisk" {
			writeFile(t, filepath.Join(m.devDir, "mmcblk0p4"), nil)
		}
		return nil
	}
	if err := m.mountDataOnly(context.Background(), o); err != nil {
		t.Fatalf("mountData: %v", err)
	}
	if got := m.mounted("/data"); got == nil || got.source == filepath.Join(m.devDir, "mmcblk0p2") {
		t.Fatalf("/data = %+v, want a partition that is not p2", got)
	}
}

func TestMountExt4ChecksOnceAfterAFailedMount(t *testing.T) {
	m := newFakeMachine(t)
	writeFile(t, filepath.Join(m.devDir, "mmcblk0p4"), ext4Image(DataLabel))
	m.mountFail["/data"] = 1
	o := m.options()

	if err := m.mountDataOnly(context.Background(), o); err != nil {
		t.Fatalf("mountData: %v", err)
	}
	if !m.ran("e2fsck -p " + filepath.Join(m.devDir, "mmcblk0p4")) {
		t.Fatalf("e2fsck was not run after a failed mount: %v", m.runs)
	}
	if m.mounted("/data") == nil {
		t.Fatal("/data was not mounted after the check")
	}
}

func TestProbeFilesystem(t *testing.T) {
	tests := []struct {
		name  string
		data  []byte
		kind  string
		label string
	}{
		{"ext4 with label", ext4Image("DATA"), "ext4", "DATA"},
		{"ext4 without label", ext4Image(""), "ext4", ""},
		{"fat", fatImage(), "fat", ""},
		{"empty", make([]byte, 8192), "empty", ""},
		{"unknown", append([]byte("junk"), make([]byte, 8192)...), "unknown", ""},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			p := filepath.Join(t.TempDir(), "dev")
			writeFile(t, p, tc.data)
			m := newFakeMachine(t)
			kind, label, err := m.options().probeFilesystem(p)
			if err != nil {
				t.Fatal(err)
			}
			if kind != tc.kind || label != tc.label {
				t.Fatalf("probeFilesystem = %q, %q; want %q, %q", kind, label, tc.kind, tc.label)
			}
		})
	}
}

func TestDiskAndPartitionNames(t *testing.T) {
	tests := []struct {
		part, disk string
		n          int
	}{
		{"/dev/mmcblk0p2", "/dev/mmcblk0", 2},
		{"/dev/mmcblk1p2", "/dev/mmcblk1", 2},
		{"/dev/nvme0n1p4", "/dev/nvme0n1", 4},
		{"/dev/sda2", "/dev/sda", 2},
		{"/dev/sdb10", "/dev/sdb", 10},
	}
	for _, tc := range tests {
		got, err := diskOf(tc.part)
		if err != nil {
			t.Fatalf("diskOf(%s): %v", tc.part, err)
		}
		if got != tc.disk {
			t.Errorf("diskOf(%s) = %s, want %s", tc.part, got, tc.disk)
		}
		if back := partOf(got, tc.n); back != tc.part {
			t.Errorf("partOf(%s, %d) = %s, want %s", got, tc.n, back, tc.part)
		}
	}
	for _, bad := range []string{"/dev/mmcblk0", "/dev/sda", "/proc/x"} {
		if _, err := diskOf(bad); err == nil {
			t.Errorf("diskOf(%s) = nil error, want a refusal", bad)
		}
	}
}

func TestRootPartitionFallsBackToCmdline(t *testing.T) {
	m := newFakeMachine(t)
	o := m.options()
	writeFile(t, m.mounts, []byte("10 1 0:5 / /proc rw - proc proc rw\n"))
	writeFile(t, filepath.Join(o.ProcDir, "cmdline"), []byte("console=ttyS0 root=/dev/mmcblk0p2 ro\n"))
	got, err := o.rootPartition()
	if err != nil {
		t.Fatal(err)
	}
	if got != "/dev/mmcblk0p2" {
		t.Fatalf("rootPartition = %s, want /dev/mmcblk0p2", got)
	}
}

// mountDataOnly is Setup without the pseudo filesystems a test has no way to
// mount.
func (m *fakeMachine) mountDataOnly(ctx context.Context, o Options) error {
	return o.withDefaults().mountData(ctx)
}

func ext4Image(label string) []byte {
	b := make([]byte, 2048)
	b[1024+0x38] = 0x53
	b[1024+0x39] = 0xEF
	copy(b[1024+0x78:], label)
	return b
}

func fatImage() []byte {
	b := make([]byte, 2048)
	copy(b[54:], "FAT16   ")
	b[510], b[511] = 0x55, 0xAA
	return b
}

func mustRead(t *testing.T, path string) []byte {
	t.Helper()
	b, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	return b
}

// record renders one helper call the way the tests compare it.
func record(name string, args []string, stdin string) string {
	s := strings.TrimSpace(name + " " + strings.Join(args, " "))
	if stdin != "" {
		s += " <<" + stdin
	}
	return s
}

func targets(specs []mountSpec) []string {
	out := make([]string, 0, len(specs))
	for _, s := range specs {
		out = append(out, s.target)
	}
	return out
}

func contains(list []string, want string) bool {
	for _, s := range list {
		if s == want {
			return true
		}
	}
	return false
}
