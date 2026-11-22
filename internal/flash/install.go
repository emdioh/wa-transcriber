package flash

import (
	"bufio"
	"context"
	"errors"
	"fmt"
	"io"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
)

// Refusals that are not about the target: each one is a sentence the console
// and the UI show as it is.
var (
	// ErrNoConfirm is the typed confirmation that was not given. The line a
	// refusal prints is "<this>: <device>", so the sentence ends on the word
	// "type" and the device follows it.
	ErrNoConfirm = errors.New("flash: type the device name to confirm")
	// ErrSameDisk is a destination that is the running disk: that clone
	// overwrites the source while reading it.
	ErrSameDisk = errors.New("flash: the destination is the disk this device booted from")
	// ErrMounted is a destination with something mounted from it.
	ErrMounted = errors.New("flash: something is mounted from the destination")
	// ErrTooSmall is a destination smaller than the bytes to copy.
	ErrTooSmall = errors.New("flash: the destination is smaller than the card's system partitions")
)

// Request is one install of the running card onto the internal storage.
type Request struct {
	// Sys is where to look for the target (zeros = board defaults).
	Sys Sys
	// Source is the running disk: "<DevDir><name>", e.g. /dev/mmcblk0.
	Source string
	// SourceName is its sysfs name (mmcblk0): it is what device nodes and
	// the refusal against cloning onto itself are computed from.
	SourceName string
	// SourceSize is the size of the running disk, used for the partition
	// sizes the region list is built from when they cannot be read.
	SourceSize int64
	// Sizes are the byte sizes of p1, p2, p3 of the source (0 = read from
	// the device nodes).
	Sizes [3]int64
	// Target is the disk to write; empty means "find it by rule" (Sys.Target).
	Target string
	// Confirm is what the owner typed: it has to equal the target's device
	// name ("/dev/mmcblk1") or its bare name ("mmcblk1").
	Confirm string
	// BootFile is the boot file's path inside the boot partition, where the
	// kernel's root is named: "extlinux/extlinux.conf" on the BeagleBone
	// Black, "cmdline.txt" on the Pi 2. The install rewrites root= there to
	// name the *target's* disk, because the clone copies the card's file
	// verbatim and the disk name changes (mmcblk0 -> mmcblk1).
	BootFile string
	// BootDir is the mounted boot partition of the *source* (/boot). It is
	// where the binary and the board settings came from; the marker itself
	// is written on the boot partition of the *target*, which Install mounts
	// off Target.Device (partition 1).
	BootDir string
	// DataDir is the running /data, where the files that make the device
	// itself are staged from (empty = /data).
	DataDir string
	// Mounts is the file a process's mount table is read from
	// (/proc/self/mounts). Empty skips the "is it mounted" check.
	Mounts string
	// Logf is where the install reports what it is doing: nil is silent. A
	// serial console is the only place an install can be watched, so every
	// step that can be slow (the clone, the read-back, the mount, each staged
	// file) says so before it starts and how long it took after.
	Logf func(format string, args ...any)
	// RereadFunc makes the kernel re-read the partition table of a disk, like
	// partprobe(8). Nil is the board's implementation (partx -u), which the
	// image has. It is best effort: a failure only makes the mount below
	// report the less specific error.
	RereadFunc func(disk string) error
	// UmountFunc detaches a mount point. Nil is the board's implementation
	// (unmount(2)); a test injects its own because nothing is really mounted.
	UmountFunc func(dir string) error
	// MountFunc mounts a device like "mount"(8) would:
	// MountFunc(device, dir, "ro") and MountFunc(dir, "", "remount,rw"). Nil
	// is the board's implementation (exec mount, which the image has).
	MountFunc func(src, dir, opts string) error
	// Stage replaces what is done inside the writable window on the target's
	// boot partition (stage the files, then write the marker), so a test can
	// exercise the no-space and nothing-to-stage paths. Nil is the real one.
	Stage func(dir string) (Stage, error)
	// Progress is called while copying and verifying.
	Progress func(done, total int64)
}

// InstallPlan is what Install would do, worked out before anything is
// written: the target, the regions and the marker. A web card renders it; a
// refusal comes back from Plan with nothing touched.
type InstallPlan struct {
	Target  Target
	Source  string
	Regions []Region
	Marker  Marker
	// Need is the bytes the clone writes (Target.Size must cover it).
	Need int64
}

// Report is the outcome of every install, successful or not.
type Report struct {
	Plan InstallPlan
	// Written is false unless every byte read back equal to the source.
	Written bool
	// Regions carries the hashes when Written is true.
	Regions []Region
	// Repointed is the target boot file's root= after the install
	// ("root=/dev/mmcblk1p2"), "" when it needed no change.
	Repointed string
	// Staged lists the files the install put on the target's boot partition
	// for the first boot to copy into /data, and Skipped those the running
	// /data does not have.
	Staged  []string
	Skipped []string
	// Unpaired is set when the copy is complete and verified but the session
	// could not be carried over (staging failed, or /data had nothing to
	// stage): the target boots, it just starts unpaired, and the message in
	// UnpairedWhy says so.
	Unpaired    bool
	UnpairedWhy string
}

// Plan works out the target and the region list and makes every refusal that
// needs no I/O: the confirmation, the same-disk check, the size. Nothing is
// written.
func Plan(req Request) (InstallPlan, error) {
	var p InstallPlan
	if req.SourceName == "" || req.Source == "" {
		return p, errors.New("flash: the running disk is unknown, refusing to clone")
	}
	p.Source = req.Source
	need, err := SourceNeed(req)
	if err != nil {
		return p, err
	}
	p.Need = need

	t, err := TargetOf(req)
	if err != nil {
		return p, err
	}
	p.Target = t
	if t.Name == req.SourceName {
		return p, fmt.Errorf("%w (%s)", ErrSameDisk, t.Device)
	}
	if t.Size > 0 && t.Size < need {
		return p, fmt.Errorf("%w: %s is %s, the copy needs %s", ErrTooSmall, t.Device, sizeMB(t.Size), sizeMB(need))
	}
	if !confirmed(req.Confirm, t.Device) {
		return p, fmt.Errorf("%w: %s", ErrNoConfirm, t.Device)
	}
	if err := NotMounted(req.Mounts, t.Device); err != nil {
		return p, err
	}
	p.Regions, err = sourceRegions(req)
	if err != nil {
		return p, err
	}
	p.Marker = NewMarker(req.SourceName)
	return p, nil
}

// TargetOf is Plan's target half: the one that was asked for, or the rule.
func TargetOf(req Request) (Target, error) {
	if req.Target == "" {
		sys := req.Sys
		sys.Need = req.SourceSize
		if sys.SysBlock == "" || sys.DevDir == "" || sys.Root == "" {
			return sys.Target()
		}
		need, err := SourceNeed(req)
		if err != nil {
			return Target{}, err
		}
		sys.Need = need
		return sys.Target()
	}
	dev := req.Target
	if !strings.HasPrefix(dev, "/") {
		dev = req.Sys.devDir() + dev
	}
	name := filepath.Base(dev)
	if name == "" || name == "." || name == "/" {
		return Target{}, fmt.Errorf("flash: %q is not a device", req.Target)
	}
	sys := req.Sys
	cands, err := disks(sys)
	if err != nil {
		return Target{}, err
	}
	for _, c := range cands {
		if c.Name != name {
			continue
		}
		if c.Running {
			return Target{}, fmt.Errorf("%w (%s)", ErrSameDisk, dev)
		}
		if c.Type != "MMC" {
			return Target{}, fmt.Errorf("flash: %s is not internal storage (type %q)", dev, c.Type)
		}
		return Target{Name: c.Name, Device: dev, Size: c.Size, Removable: c.Removable}, nil
	}
	return Target{}, fmt.Errorf("%w: %s is not a disk of this machine: %s", ErrNoTarget, dev, candidates(cands))
}

// SourceNeed is the number of bytes the copy covers, from the partition sizes.
func SourceNeed(req Request) (int64, error) {
	regions, err := sourceRegions(req)
	if err != nil {
		return 0, err
	}
	return Need(regions), nil
}

// sourceRegions builds the region list of the running card: MBR, boot, root A
// and root B, each at the offset the previous one ends at (the card layout is
// contiguous, D18).
func sourceRegions(req Request) ([]Region, error) {
	sizes := req.Sizes
	for i := range sizes {
		if sizes[i] > 0 {
			continue
		}
		dev := req.Sys.devDir() + partitionDev(req.SourceName, i+1)
		n := deviceSize(dev)
		if n <= 0 {
			return nil, fmt.Errorf("flash: cannot read the size of %s", dev)
		}
		sizes[i] = n
	}
	r := Regions(512, sizes[0], sizes[1], sizes[2])
	if r == nil {
		return nil, errors.New("flash: the running card has no readable partition sizes")
	}
	return r, nil
}

// NotMounted refuses a destination with something mounted from it: cloning
// onto a mounted filesystem is a correct-looking write under a live one.
func NotMounted(mountsFile, dev string) error {
	if mountsFile == "" {
		return nil
	}
	f, err := os.Open(mountsFile)
	if err != nil {
		// No mount table is not a reason to refuse; the caller that cares
		// passes a file, and a board always has one.
		return nil
	}
	defer f.Close()
	base := filepath.Base(dev)
	sc := bufio.NewScanner(f)
	sc.Buffer(make([]byte, 64<<10), 64<<10)
	for sc.Scan() {
		fields := strings.Fields(sc.Text())
		if len(fields) < 2 {
			continue
		}
		src := fields[0]
		// A partition of the destination counts as the destination:
		// /dev/mmcblk1p2 is written by the same clone.
		if src == dev || filepath.Base(src) == base || strings.HasPrefix(filepath.Base(src), base+"p") {
			return fmt.Errorf("%w: %s is mounted on %s", ErrMounted, src, fields[1])
		}
	}
	return nil
}

// confirmed accepts the device path or its bare name, and nothing else: a
// checkbox is one keystroke away from a click, a disk name has to be read.
func confirmed(got, dev string) bool {
	g := strings.TrimSpace(got)
	return g != "" && (g == dev || g == filepath.Base(dev))
}

// Install runs the plan: opens the source and the destination, clones the
// regions and reads them back. Then, only if that is clean, it mounts the
// target's boot partition read-write, stages the /data files into MigrateDir
// there, writes the marker last and unmounts again.
//
// It returns a Report for every outcome. An error means: the card still boots
// the device and the target is not bootable yet — rerun it. Nothing half-done
// is ever reported as done, and the source is only ever read. A copy that is
// complete but could not carry the session over is not an error: it is
// Written with Unpaired set, because the target boots (just unpaired).
func Install(ctx context.Context, req Request) (Report, error) {
	var rep Report
	p, err := Plan(req)
	if err != nil {
		return rep, err
	}
	rep.Plan = p

	src, err := os.Open(req.Source)
	if err != nil {
		return rep, fmt.Errorf("flash: opening the source %s: %w", req.Source, err)
	}
	defer src.Close()
	dst, err := os.OpenFile(p.Target.Device, os.O_RDWR, 0)
	if err != nil {
		return rep, fmt.Errorf("flash: opening %s: %w", p.Target.Device, err)
	}
	defer dst.Close()

	regions, cerr := Clone(ctx, src, dst, p.Regions, req.Progress)
	rep.Regions = regions
	if cerr != nil {
		return rep, cerr
	}
	if err := dst.Sync(); err != nil {
		return rep, &ErrNotWritten{Region: "the copy", Err: fmt.Errorf("flushing %s: %w", p.Target.Device, err)}
	}

	// The session travels with the clone. It is staged on the boot partition
	// of the *target*, because the first boot from that disk is the one after
	// the owner took the card out — the old format named the card's DATA here,
	// and on the board that device is simply not there at that moment. The
	// marker goes last: a marker on a boot partition whose staged files are
	// not all there yet would make that first boot copy half a session.
	stage, serr := OnTargetBoot(req, p.Target, req.stageFunc(p.Marker, p.Target))
	rep.Written = true
	rep.Staged, rep.Skipped = stage.Files, stage.Skipped
	rep.Repointed = stage.Repointed
	if serr != nil {
		rep.Unpaired = true
		rep.UnpairedWhy = fmt.Sprintf("the session could not be staged (%v), so the device will boot unpaired: "+
			"its settings, WhatsApp session and WiFi networks stay on the card", serr)
	} else if len(rep.Staged) == 0 {
		rep.Unpaired = true
		rep.UnpairedWhy = fmt.Sprintf("there was nothing in %s to carry over, so the device will boot unpaired",
			req.dataDir())
	}
	return rep, nil
}

// OnTargetBoot makes the target's boot partition (<disk>p1) writable, hands
// fn its mount point and unmounts it again whatever fn did. It is the only
// part of the install that needs a kernel: fn gets a directory, so the
// staging and the marker are testable without mounting anything.
//
// The write went through the whole-disk node, and the kernel only re-reads the
// partition table that came with the clone on the next boot; the boot
// partition is therefore mounted with the crc32 VFAT driver first, which takes
// the offsets from the partition entry itself.
func OnTargetBoot(req Request, t Target, fn func(mountPoint string) (Stage, error)) (Stage, error) {
	logf := req.logf()
	var st Stage
	if t.Name == "" || t.Device == "" {
		return st, errors.New("flash: the target has no boot partition to stage on")
	}
	bootDev := req.Sys.devDir() + partitionDev(t.Name, 1)
	// The clone rewrote the target's partition table; the kernel still has the
	// old one in memory. Without this the mount below reads the previous
	// layout's bytes and fails.
	logf("flash: re-reading the partition table of %s", t.Device)
	rerr := req.rereadFunc()(t.Device)
	if rerr != nil {
		logf("flash: re-reading the table failed (%v), trying the mount anyway", rerr)
	}
	dir := filepath.Join(mountRoot(), "wat-stage")
	if err := os.MkdirAll(dir, 0o755); err != nil {
		return st, fmt.Errorf("flash: creating %s: %w", dir, err)
	}
	mount := req.mountFunc()
	var merr error
	mounted := false
	for _, src := range []string{"vfat:" + bootDev, bootDev} {
		logf("flash: mounting %s on %s read-write", src, dir)
		if merr = mount(src, dir, "rw"); merr == nil {
			mounted = true
			logf("flash: mounted %s", dir)
			break
		}
		logf("flash: mount failed: %v", merr)
	}
	if !mounted {
		os.Remove(dir)
		if rerr != nil {
			return st, fmt.Errorf("flash: mounting the target's boot partition read-write: %w (after %v)", merr, rerr)
		}
		return st, fmt.Errorf("flash: mounting the target's boot partition read-write: %w", merr)
	}
	// Whatever happens inside, the mount point goes away again: the staging
	// directory belongs to this run.
	defer func() {
		// Read-only first, then detached: a remount alone leaves the boot
		// partition mounted, which makes the next install refuse (it must not
		// write under a live mount) and keeps the FAT writable for no reason.
		logf("flash: remounting %s read-only", dir)
		if err := mount(dir, "", "remount,ro"); err != nil {
			logf("flash: remount read-only failed: %v", err)
		}
		logf("flash: unmounting %s", dir)
		if err := req.umountFunc()(dir); err != nil {
			logf("flash: unmounting %s failed: %v (it stays mounted until the reboot)", dir, err)
		}
		os.Remove(dir)
	}()
	return fn(dir)
}

// mountRoot is where the boot partition is mounted for the staging: /run is a
// tmpfs on every image. A process that cannot write there (a test, an
// unprivileged run) gets a private directory instead: the staging is not
// less correct, and the target is the only thing that matters.
func mountRoot() string {
	if d, err := os.MkdirTemp("/run", "wat-stage-*"); err == nil {
		return filepath.Dir(d)
	}
	return os.TempDir()
}

// dataDir is where the files that make the device itself are read from.
func (r Request) dataDir() string {
	if r.DataDir != "" {
		return r.DataDir
	}
	return "/data"
}

// stageFunc is what runs inside the writable window on the target's boot
// partition: the files into MigrateDir, then the marker listing them, last.
// The request's own Stage wins (a test).
func (r Request) stageFunc(m Marker, t Target) func(dir string) (Stage, error) {
	if r.Stage != nil {
		return r.Stage
	}
	return func(dir string) (Stage, error) {
		st, err := stageFiles(r.dataDir(), filepath.Join(dir, MigrateDir), MigrateFiles,
			freeSpace(dir), blockSize(dir), r.logf())
		if err != nil {
			return st, err
		}
		// The boot file must name the target's disk, not the card's: the
		// clone copied it verbatim. Do it before the marker, so a power cut
		// leaves no marker until the target is consistent.
		logf := r.logf()
		if token, err := repointBoot(dir, r.BootFile, "/dev/"+r.SourceName, t.Device); err != nil {
			return st, err
		} else if token != "" {
			st.Repointed = token
			logf("flash: the target's boot file now says %s", token)
		} else {
			logf("flash: the target's boot file already points at %s, left alone", t.Device)
		}
		if len(st.Files) == 0 {
			return st, nil
		}
		// Only with files behind it: S01data copies from the staged
		// directory and deletes the marker afterwards, so a marker with
		// nothing staged would be a first boot that copies nothing.
		m.Files = st.Files
		if err := WriteMarker(dir, m, nil); err != nil {
			return st, err
		}
		return st, nil
	}
}

// rereadFunc re-reads the partition table after the clone: the kernel still
// holds the table the target had before, whose offsets no longer match what
// was just written, so mounting <disk>p1 reads the old layout (on the board
// that is EINVAL). partx -u is in the image (S01data uses it).
func (r Request) rereadFunc() func(disk string) error {
	if r.RereadFunc != nil {
		return r.RereadFunc
	}
	return func(disk string) error {
		out, err := exec.Command("partx", "-u", disk).CombinedOutput()
		if err != nil {
			return fmt.Errorf("partx -u %s: %w: %s", disk, err, strings.TrimSpace(string(out)))
		}
		return nil
	}
}

// umountFunc detaches the staging mount: the request's own, or unmount(2).
func (r Request) umountFunc() func(dir string) error {
	if r.UmountFunc != nil {
		return r.UmountFunc
	}
	return unmount
}

// logf is the install's log sink: the request's own, or silence.
func (r Request) logf() func(string, ...any) {
	if r.Logf != nil {
		return r.Logf
	}
	return func(string, ...any) {}
}

// mountFunc is the mount the staging uses: the request's own, or mount(8).
func (r Request) mountFunc() func(src, dir, opts string) error {
	if r.MountFunc != nil {
		return r.MountFunc
	}
	return func(src, dir, opts string) error {
		args := []string{"-o", opts}
		target := dir
		if dir == "" {
			// The remount form: MountFunc(dir, "", "remount,ro"), as the
			// interface above documents and the staging cleanup uses. mount(8)
			// wants `mount -o remount,ro <dir>` with no second argument, so
			// passing both was an EINVAL ("No such file or directory") that
			// left the boot partition mounted read-write.
			target = src
		} else if src != "" {
			args = append(args, src)
		}
		out, err := exec.Command("mount", append(args, target)...).CombinedOutput()
		if err != nil {
			return fmt.Errorf("mount -o %s %s %s: %w: %s", opts, src, dir, err, strings.TrimSpace(string(out)))
		}
		return nil
	}
}

// deviceSize is the size of a block device or file (0 when unknown).
func deviceSize(path string) int64 {
	f, err := os.Open(path)
	if err != nil {
		return 0
	}
	defer f.Close()
	n, err := f.Seek(0, io.SeekEnd)
	if err != nil || n < 0 {
		return 0
	}
	return n
}
