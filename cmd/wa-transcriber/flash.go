// Install the running card onto the board's internal flash (M11,
// ARCHITECTURE.md section 11). This is the console half: -flash <device>
// clones, verifies and exits, with the progress on stderr, because the web
// UI does not survive the reboot that follows. The service behind the System
// page's "Internal storage" card is flashService below.
//
// internal/flash owns the rule that finds the eMMC, the clone, the read-back
// and the migration marker; nothing here knows where a partition starts.
package main

import (
	"context"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"os"
	"os/signal"
	"strings"
	"sync"
	"syscall"
	"time"

	"github.com/emdioh/wa-transcriber/internal/flash"
	"github.com/emdioh/wa-transcriber/internal/web"
)

const (
	// flashProgressEvery is the console reporting step: one line every 16 MB
	// of a 4 GB clone is 256 lines, which a serial console keeps up with.
	flashProgressEvery = 16 << 20
	// flashMarkerName is /boot/wat-migrate: the marker S01data reads.
	flashMarkerName = "wat-migrate"
)

// flashSource describes the disk this device booted from, in the terms
// flash.Request wants: the running root's partition from sysfs, its parent
// disk, and the sizes of the three system partitions.
//
// Layout is the board's image settings file, read for the partition names only
// (p2, p3); the card layout itself is p1 boot, p2 root A, p3 root B, p4 DATA
// (D18). It returns the running disk's sysfs name and device node.
func flashSource(req imageRequest) (name, dev string, sizes [3]int64, err error) {
	l, err := imageLayout(req)
	if err != nil {
		return "", "", sizes, err
	}
	slots, err := l.FindSlots()
	if err != nil {
		return "", "", sizes, fmt.Errorf("this device does not run from the card layout the install copies: %w", err)
	}
	name = slots.Disk
	dev = req.DevDir + name
	// p1 boot, then the two root slots; DATA is not copied (it is created on
	// the new disk and filled by the migration marker).
	bootPart := partitionOf(name, 1)
	bootDev := req.DevDir + bootPart
	sizes[0] = fileSize(bootDev)
	sizes[1] = slots.Both[0].Size
	sizes[2] = slots.Both[1].Size
	return name, dev, sizes, nil
}

// partitionOf is the device name of partition n on diskName (mmcblk0p1,
// sda1, nvme0n1p1): the rule sd.sh's wat_part follows.
func partitionOf(diskName string, n int) string {
	switch {
	case strings.HasPrefix(diskName, "mmcblk"), strings.HasPrefix(diskName, "nvme"), strings.HasPrefix(diskName, "loop"):
		return fmt.Sprintf("%sp%d", diskName, n)
	}
	return fmt.Sprintf("%s%d", diskName, n)
}

func fileSize(path string) int64 {
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

// flashPaths is where a flash install looks: the board defaults, with every
// one of them separately replaceable (a test, a dev machine without /boot).
type flashPaths struct {
	SysBlock string
	DevDir   string
	BootDir  string
	DataDir  string
	Mounts   string
	Root     string
	// MountFunc replaces exec mount(8) (a test).
	MountFunc func(src, dir, opts string) error
	// Logf is where the install reports each step: the -flash mode sets the
	// logger that writes to the console, the web card the server's logger.
	Logf func(format string, args ...any)
}

// bootFileInPartition is the boot file's path *inside* the boot partition
// ("extlinux/extlinux.conf", "cmdline.txt"), from the board's image settings.
// The install rewrites root= in it so the clone names the target's disk.
func bootFileInPartition(req imageRequest, bootDir string) (string, error) {
	s, err := readImageSettings(req.Layout)
	if err != nil {
		return "", err
	}
	// The boot partition is mounted at bootDir (the tests use a temp dir), so
	// the file's path inside it is what follows that prefix.
	prefix := strings.TrimSuffix(bootDir, "/") + "/"
	if rel, ok := strings.CutPrefix(s.BootFile, prefix); ok && rel != "" {
		return rel, nil
	}
	return "", fmt.Errorf("the boot file %q is not inside %s", s.BootFile, bootDir)
}

// flashRequest builds the flash.Request of an install to target ("" = the
// rule's answer).
func flashRequest(imgReq imageRequest, paths flashPaths, target, confirm string,
	progress func(done, total int64)) (flash.Request, error) {
	var req flash.Request
	name, dev, sizes, err := flashSource(imgReq)
	if err != nil {
		return req, err
	}
	bootFile, berr := bootFileInPartition(imgReq, paths.BootDir)
	if berr != nil {
		return req, berr
	}
	req = flash.Request{
		BootFile:   bootFile,
		Logf:       paths.Logf,
		Sys:        flash.Sys{SysBlock: paths.SysBlock, DevDir: paths.DevDir, Root: paths.Root},
		Source:     dev,
		SourceName: name,
		Sizes:      sizes,
		Target:     target,
		Confirm:    confirm,
		BootDir:    paths.BootDir,
		DataDir:    paths.DataDir,
		Mounts:     paths.Mounts,
		MountFunc:  paths.MountFunc,
		Progress:   progress,
	}
	return req, nil
}

// runFlashCmd is -flash in main: the board defaults, and a signal turns into
// the cancellation flash.Clone reports per region.
func runFlashCmd(dev, confirm, bootDir, dataDir, layoutFile string) error {
	ctx, stop := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
	defer stop()
	imgReq := imageRequest{
		Dir:      dataDir,
		Layout:   layoutFile,
		SysBlock: defaultSysBlock,
		DevDir:   defaultDevDir,
	}
	paths := flashPaths{
		SysBlock: defaultSysBlock,
		DevDir:   defaultDevDir,
		BootDir:  bootDir,
		DataDir:  dataDir,
		Mounts:   "/proc/self/mounts",
	}
	if code := runFlash(ctx, imgReq, paths, dev, confirm, os.Stdout, os.Stderr); code != 0 {
		return errFlashFailed
	}
	return nil
}

// errFlashFailed is main's non-zero exit for -flash; runFlash has already
// printed what happened and which device is still bootable.
var errFlashFailed = errors.New("install to the internal storage did not complete")

// runFlash is the -flash mode: work out the target, refuse early and loudly
// when it is not an accepted one, then clone with the progress on stderr and
// exit. It never starts the transcription service: the UI does not survive
// the reboot, the console does.
//
// exitCode is 0 only when every byte read back equal to the card. Every other
// outcome says, in one line, that the card still boots the device and the
// internal storage is not bootable yet.
func runFlash(ctx context.Context, imgReq imageRequest, paths flashPaths, target, confirm string,
	stdout, stderr io.Writer) int {
	log := slog.New(slog.NewTextHandler(stderr, &slog.HandlerOptions{Level: slog.LevelInfo}))
	// Every step of the install goes to the console as it happens: the point
	// of this mode is that somebody is watching a serial line.
	paths.Logf = func(format string, args ...any) { log.Info(fmt.Sprintf(format, args...)) }

	// The target, before anything else: the whole point of the rule is that
	// a device nobody can name is not written.
	req, err := flashRequest(imgReq, paths, target, confirm, nil)
	if err != nil {
		fmt.Fprintln(stderr, "wa-transcriber: -flash:", err)
		return 2
	}
	plan, err := flash.Plan(req)
	if err != nil {
		fmt.Fprintln(stderr, "wa-transcriber: -flash:", err)
		if errors.Is(err, flash.ErrNoConfirm) {
			fmt.Fprintf(stderr, "wa-transcriber: -flash: rerun with -flash-confirm %s\n", plan.Target.Device)
		}
		return 2
	}
	fmt.Fprintf(stdout, "Card %s → internal storage %s (%s), %s to copy\n",
		req.Source, plan.Target.Device, flashSize(plan.Target.Size), flashSize(plan.Need))
	for _, r := range plan.Regions {
		fmt.Fprintf(stdout, "  %-7s at %10d  %s\n", r.Name, r.Offset, flashSize(r.Size))
	}
	fmt.Fprintf(stdout, "Migration marker: %s lists %s, staged in %s/ on the internal storage\n",
		flashMarkerName, strings.Join(plan.Marker.Files, ", "), flash.MigrateDir)

	// The progress: one line every 16 MB plus the start of each region, so a
	// serial console shows movement and the log keeps a record.
	var mu sync.Mutex
	var last int64 = -flashProgressEvery
	start := time.Now()
	progress := func(done, total int64) {
		mu.Lock()
		defer mu.Unlock()
		if done-last < flashProgressEvery && done < total {
			return
		}
		last = done
		pct := 0.0
		if total > 0 {
			pct = 100 * float64(done) / float64(total)
		}
		fmt.Fprintf(stderr, "-flash: %3.0f%%  %s of %s  %s\n", pct, flashSize(done), flashSize(total), time.Since(start).Round(time.Second))
	}
	req.Progress = progress

	// A cancelled clone (Ctrl-C) is reported like any other failure: the
	// card still boots and the internal storage is not bootable yet.
	rep, err := flash.Install(ctx, req)
	if err != nil {
		log.Error("install to internal storage failed", "err", err)
		fmt.Fprintln(stderr, "wa-transcriber: -flash: the internal storage is NOT bootable yet; the card still boots the device.")
		fmt.Fprintln(stderr, "wa-transcriber: -flash: fix the error above and run it again (every byte is a copy, so it is repeatable).")
		return 1
	}
	for _, r := range rep.Regions {
		fmt.Fprintf(stdout, "  %-7s sha256 %s\n", r.Name, r.SHA256)
	}
	fmt.Fprintln(stdout, "The copy is written and reads back identical.")
	if rep.Unpaired {
		fmt.Fprintln(stdout, "The session was NOT carried over: "+rep.UnpairedWhy+".")
		fmt.Fprintln(stdout, "Power the device off and remove the SD card when you are ready: it boots from the internal storage")
		fmt.Fprintln(stdout, "unpaired (it asks for the phone again). Keep the card: its /data still has the pairing.")
		return 0
	}
	fmt.Fprintf(stdout, "The settings, the WhatsApp session and the WiFi networks are staged on its boot partition (%s/, %s).\n",
		flash.MigrateDir, strings.Join(rep.Staged, ", "))
	fmt.Fprintln(stdout, "Power the device off, REMOVE THE SD CARD, and power it on again: it boots from the internal storage,")
	fmt.Fprintln(stdout, "copies them into its own /data once, and stays paired.")
	return 0
}

// flashSize renders bytes for the console: "412 MB", "4 GB".
func flashSize(n int64) string {
	if n <= 0 {
		return "unknown"
	}
	const mb = 1 << 20
	if n%(1<<30) == 0 {
		return fmt.Sprintf("%d GB", n>>30)
	}
	return fmt.Sprintf("%.1f MB", float64(n)/mb)
}

// flashService is web.FlashInstaller over internal/flash: the "Internal
// storage" card. It is separate from imageService on purpose — that one
// writes a file into a slot of the running disk, this one writes another
// disk — and it shares only the boot partition helper.
type flashService struct {
	imgReq imageRequest
	paths  flashPaths
	log    *slog.Logger

	mu  sync.Mutex
	run bool // an install is running
	// The card on /system shows these: how far the running install got, and
	// the outcome of the last one. Both live only in this process, which is
	// the lifetime that matters - the page that starts an install is not the
	// page that sees it end, and after a reboot the answer is on the boot
	// partition, not here.
	done   int64
	total  int64
	doneAt time.Time
}

// newFlashService builds the card's service. Nil (no card at all) when the
// image update is off: the install needs the same board layout file, and a
// device without it (Docker) has no internal flash to install to.
func newFlashService(imgReq imageRequest, paths flashPaths, log *slog.Logger) *flashService {
	if imgReq.Dir == "" {
		return nil
	}
	if log == nil {
		log = slog.Default()
	}
	return &flashService{imgReq: imgReq, paths: paths, log: log}
}

// Status implements web.FlashInstaller: the detected target and its size, or
// the one line a board without internal flash gets.
func (s *flashService) Status(ctx context.Context) web.FlashStatus {
	req, err := flashRequest(s.imgReq, s.paths, "", "", nil)
	if err != nil {
		return web.FlashStatus{Enabled: true, Error: flashStatusError(err)}
	}
	need, nerr := flash.SourceNeed(req)
	if nerr != nil {
		return web.FlashStatus{Enabled: true, Error: flashStatusError(nerr)}
	}
	req.Sys.Need = need
	t, err := flash.TargetOf(req)
	if err != nil {
		return web.FlashStatus{Enabled: true, Error: flashStatusError(err)}
	}
	s.mu.Lock()
	running := s.run
	done, total, doneAt := s.done, s.total, s.doneAt
	s.mu.Unlock()
	st := web.FlashStatus{
		Enabled:       true,
		Target:        t.Device,
		Size:          t.Size,
		Need:          need,
		Source:        req.Source,
		Marker:        flashMarkerName,
		Install:       t.Name,
		Busy:          running,
		ProgressDone:  done,
		ProgressTotal: total,
		Done:          !doneAt.IsZero(),
		DoneAt:        doneAt,
	}
	return st
}

// flashStatusError turns a refusal into the card's one line: a board without
// internal flash says exactly that, anything else says what happened.
func flashStatusError(err error) string {
	if errors.Is(err, flash.ErrNoTarget) && !strings.Contains(err.Error(), "refusing to pick") {
		return "This board has no internal storage: the only flash it has is the card it boots from."
	}
	return err.Error()
}

// Install implements web.FlashInstaller: clone the card onto the internal
// storage, verify it, write the marker. The progress goes to the log buffer,
// which the Logs page shows.
func (s *flashService) Install(ctx context.Context, confirm string) (web.FlashStatus, error) {
	s.mu.Lock()
	if s.run {
		s.mu.Unlock()
		return s.Status(ctx), errors.New("an install is already running")
	}
	s.run = true
	s.done, s.total = 0, 0
	s.doneAt = time.Time{}
	s.mu.Unlock()
	defer func() {
		s.mu.Lock()
		s.run = false
		s.done, s.total = 0, 0
		s.mu.Unlock()
	}()

	var last int64 = -flashProgressEvery
	progress := func(done, total int64) {
		// The numbers the page reads are updated on every chunk; the log line
		// stays at one every 16 MB so the ring buffer is not flooded.
		s.mu.Lock()
		s.done, s.total = done, total
		s.mu.Unlock()
		if done-last < flashProgressEvery && done < total {
			return
		}
		last = done
		s.log.Info("flash: copying to the internal storage", "done", done, "total", total)
	}
	req, err := flashRequest(s.imgReq, s.paths, "", confirm, progress)
	if err != nil {
		return s.Status(ctx), err
	}
	rep, err := flash.Install(ctx, req)
	if err != nil {
		return s.Status(ctx), err
	}
	s.log.Warn("flash: internal storage written and verified", "target", rep.Plan.Target.Device,
		"bytes", rep.Plan.Need, "marker", flashMarkerName, "staged", strings.Join(rep.Staged, ", "))
	if rep.Unpaired {
		s.log.Warn("flash: the device will boot unpaired", "why", rep.UnpairedWhy)
	}
	s.mu.Lock()
	s.doneAt = time.Now()
	s.mu.Unlock()
	return s.Status(ctx), nil
}

var _ web.FlashInstaller = (*flashService)(nil)
