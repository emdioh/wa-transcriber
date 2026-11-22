package flash

import (
	"bufio"
	"context"
	"errors"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"strings"
)

// Refusals that are not about the target: each one is a sentence the console
// and the UI show as it is.
var (
	// ErrNoConfirm is the typed confirmation that was not given.
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
	// BootDir is the mounted boot partition of the *source* (/boot), where
	// the migration marker is written.
	BootDir string
	// Mounts is the file a process's mount table is read from
	// (/proc/self/mounts). Empty skips the "is it mounted" check.
	Mounts string
	// Progress is called while copying and verifying.
	Progress func(done, total int64)
	// WithBootRW makes the boot partition writable for the marker.
	WithBootRW func() (func() error, error)
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
		return p, fmt.Errorf("%w: type %s", ErrNoConfirm, t.Device)
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
// regions, reads them back and, only then, writes the migration marker on the
// source's boot partition.
//
// It returns a Report for every outcome. An error with Written=false means:
// the card still boots the device and the target is not bootable yet — rerun
// it. Nothing half-done is ever reported as done, and the source is only ever
// read except for the marker file.
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
	// The marker goes on last, after the read-back: a marker written before
	// the clone completed would make the first boot on the empty internal
	// storage look for /data on a partition that is not there yet. A failure
	// here is not a failed clone — the target boots, it just starts unpaired.
	if err := WriteMarker(req.BootDir, p.Marker, req.WithBootRW); err != nil {
		return rep, fmt.Errorf("flash: the copy is complete and verified, but the migration marker could not be written "+
			"(the device will boot unpaired): %w", err)
	}
	rep.Written = true
	return rep, nil
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
