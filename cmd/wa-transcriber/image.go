// Image (OS) updates from the web UI (M10, D18, ARCHITECTURE.md section 11).
// This is the counterpart of update.go for the second root slot: the boot
// counter, the service behind web.ImageUpdater, the remount of /boot that a
// switch needs, and the health timer that confirms the binary and the slot.
// internal/imageupdate owns the write, the read-back, the boot file and the
// state on /data; nothing here knows what a partition looks like.
package main

import (
	"context"
	"crypto/ed25519"
	"encoding/json"
	"errors"
	"fmt"
	"log/slog"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"time"

	"github.com/emdioh/wa-transcriber/internal/imageupdate"
	"github.com/emdioh/wa-transcriber/internal/update"
	"github.com/emdioh/wa-transcriber/internal/web"
)

// imageRequest is everything the image update needs to find the device. An
// empty Dir is the feature off (Docker, a dev machine).
type imageRequest struct {
	// Dir is -image-update: the data directory, where image-boot.json lives
	// (/data on a board).
	Dir string
	// Layout is -image-layout: a file from the image describing the board,
	// because the same armv7 binary runs on two boards with different
	// bootloaders. See imageSettings.
	Layout string
	// SysBlock and DevDir are where partitions are announced and where their
	// device nodes are; a test points them at temp files.
	SysBlock string
	DevDir   string
	// Root is the mount point that identifies the running root; empty means
	// "/". A test has no / to stat.
	Root string
	// BootRW makes the boot partition writable for one write; nil means
	// mount(2) remount on /boot.
	BootRW func() (func() error, error)
}

// Defaults on a board.
const (
	defaultImageLayout = "/etc/wat/image.json"
	defaultSysBlock    = "/sys/class/block"
	defaultDevDir      = "/dev/"
	// imageDownloadSlack is how far above the manifest size the HTTP length
	// may go before the download is refused.
	imageDownloadSlack = 1 << 20
)

// imageSettings is the root-owned description of the A/B layout of one board
// (/etc/wat/image.json). The boot file names the running root:
// /boot/extlinux/extlinux.conf on the BeagleBone Black, /boot/cmdline.txt on
// the Pi 2.
//
//	{"board": "beagle", "boot_file": "/boot/extlinux/extlinux.conf",
//	 "slots": ["p2", "p3"]}
type imageSettings struct {
	// Board is the "images" key of this board in the release manifest.
	Board    string    `json:"board"`
	BootFile string    `json:"boot_file"`
	Slots    [2]string `json:"slots"`
}

// readImageSettings reads and checks the board description. A missing or
// broken file is the old layout as far as the UI is concerned: the card says
// "reflash once", which is also the fix for a missing file.
func readImageSettings(path string) (imageSettings, error) {
	var s imageSettings
	if path == "" {
		return s, errors.New("no -image-layout")
	}
	b, err := os.ReadFile(path)
	if err != nil {
		return s, fmt.Errorf("reading %s: %w", path, err)
	}
	if err := json.Unmarshal(b, &s); err != nil {
		return s, fmt.Errorf("parsing %s: %w", path, err)
	}
	if s.BootFile == "" || s.Slots[0] == "" || s.Slots[1] == "" || s.Slots[0] == s.Slots[1] {
		return s, fmt.Errorf("%s does not describe two root slots and a boot file", path)
	}
	return s, nil
}

// imageLayout builds the imageupdate.Layout of the running device.
func imageLayout(req imageRequest) (imageupdate.Layout, error) {
	s, err := readImageSettings(req.Layout)
	if err != nil {
		return imageupdate.Layout{}, err
	}
	root := req.Root
	if root == "" {
		root = "/"
	}
	return imageupdate.Layout{
		Root:     root,
		BootDir:  "/boot",
		BootFile: s.BootFile,
		DataDir:  req.Dir,
		SysBlock: req.SysBlock,
		DevDir:   req.DevDir,
		Slots:    s.Slots,
	}, nil
}

// bootRW is the scope imageupdate.Service.BootRW and BootIn want: make /boot
// writable, hand back the way out. A request with its own helper (a test, or a
// board that mounts /boot differently) wins.
func bootRW(req imageRequest) func() (func() error, error) {
	if req.BootRW != nil {
		return req.BootRW
	}
	dir := "/boot"
	return func() (func() error, error) {
		if err := remountBoot(dir, true); err != nil {
			return nil, err
		}
		return func() error { return remountBoot(dir, false) }, nil
	}
}

// imageBoot is the image half of the boot gate. It runs next to update.Boot,
// before the config and WhatsApp: it counts a start of a slot that has not
// confirmed itself and, after imageupdate.MaxBootTries, moves the boot file
// back and asks for a reboot.
//
// It never fails the process: a device that cannot count its boots keeps
// running what it runs, exactly as before the feature existed.
func imageBoot(req imageRequest, log *slog.Logger, now time.Time) imageupdate.BootAction {
	if req.Dir == "" {
		return imageupdate.BootAction{Note: "off"}
	}
	l, err := imageLayout(req)
	if err != nil {
		log.Warn("imageupdate: no A/B layout, the boot counter is off", "err", err)
		return imageupdate.BootAction{Note: "no layout"}
	}
	act, err := imageupdate.BootIn(l, now, bootRW(req))
	if err != nil {
		log.Warn("imageupdate: boot counter", "err", err)
	}
	return act
}

// imageService is web.ImageUpdater over internal/imageupdate: the signed
// manifest, the download into the inactive slot and the two buttons. One mutex
// serialises them, so two clicks cannot interleave two writes.
type imageService struct {
	req     imageRequest
	svc     *imageupdate.Service // nil on an old layout
	board   string
	dl      *imageupdate.Downloader
	checker *update.Checker
	current string
	busy    func() bool
	log     *slog.Logger

	mu        sync.Mutex
	latest    *update.Manifest
	checkedAt time.Time
}

// newImageService builds the service; nil means the feature is off and main
// passes nothing to web.Options, so the card is absent. key is the same
// ed25519 key the binary update uses: one manifest, one signature.
func newImageService(req imageRequest, current, manifestURL string, key ed25519.PublicKey,
	busy func() bool, log *slog.Logger) *imageService {
	if req.Dir == "" {
		return nil
	}
	if log == nil {
		log = slog.Default()
	}
	s := &imageService{
		req:     req,
		board:   "beagle",
		dl:      &imageupdate.Downloader{Slack: imageDownloadSlack},
		checker: &update.Checker{ManifestURL: manifestURL, Pub: key, CachePath: filepath.Join(req.Dir, "updates", "check.json")},
		current: current,
		busy:    busy,
		log:     log,
	}
	if key == nil {
		log.Info("imageupdate: enabled only in a build that can verify the manifest")
	}
	if l, err := imageLayout(req); err != nil {
		// An old image has no layout file: the card says "reflash once".
		log.Info("imageupdate: the card will ask for a reflash", "err", err)
	} else {
		s.svc = &imageupdate.Service{Layout: l, Log: log, BootRW: bootRW(req)}
		if set, err := readImageSettings(req.Layout); err == nil && set.Board != "" {
			s.board = set.Board
		}
	}
	// A previous run may have left a verified manifest: the card shows what
	// was available without a request.
	s.latest = s.checker.Cached()
	if s.latest != nil {
		s.checkedAt = time.Now()
	}
	return s
}

// Status implements web.ImageUpdater.
func (s *imageService) Status(context.Context) web.ImageStatus {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.status()
}

// Check implements web.ImageUpdater: fetch and verify the newest manifest and
// report which image this board could install.
func (s *imageService) Check(ctx context.Context) (web.ImageStatus, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	m, err := s.checker.Refresh(ctx)
	if err != nil {
		return s.status(), err
	}
	s.latest = m
	s.checkedAt = time.Now()
	return s.status(), nil
}

// Install implements web.ImageUpdater: download the board's image into the
// inactive slot, verify it by read-back, then move the boot file. All or
// nothing: a failure before the boot file leaves the device running exactly
// what it runs now.
func (s *imageService) Install(ctx context.Context) (web.ImageStatus, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	if err := s.ready(); err != nil {
		return s.status(), err
	}
	if s.busy != nil && s.busy() {
		return s.status(), errors.New("a voice note is queued or running, try again in a minute")
	}
	if s.latest == nil {
		m, err := s.checker.Latest(ctx)
		if err != nil {
			return s.status(), err
		}
		s.latest = m
		s.checkedAt = time.Now()
	}
	img, ok := s.image(s.latest)
	if !ok {
		return s.status(), fmt.Errorf("%s carries no image for this board", s.latest.Version)
	}
	if img.NeedsLayout() {
		// Never download 200 MB to find out the disk layout is wrong.
		return s.status(), errors.New("the release needs a newer partition layout: reflash once to enable image updates")
	}
	s.log.Warn("imageupdate: downloading", "version", s.latest.Version, "file", img.File, "bytes", img.Size)
	if _, err := s.dl.Fetch(ctx, s.assetURL(img), s.svc, img.Size, img.SHA256); err != nil {
		return s.status(), err
	}
	if err := s.svc.Switch(s.latest.Version); err != nil {
		return s.status(), err
	}
	s.log.Warn("imageupdate: image installed on the other slot, reboot needed", "version", s.latest.Version)
	return s.status(), nil
}

// Rollback implements web.ImageUpdater: point the boot file at the previous
// slot again. The caller reboots.
func (s *imageService) Rollback(context.Context) (web.ImageStatus, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	if err := s.ready(); err != nil {
		return s.status(), err
	}
	if err := s.svc.Rollback(); err != nil {
		return s.status(), err
	}
	s.log.Warn("imageupdate: going back to the previous image")
	return s.status(), nil
}

// ready is the check every action makes: an old layout has nothing to write.
func (s *imageService) ready() error {
	if s.svc == nil {
		return errors.New("this image has one root partition: reflash once to enable image updates")
	}
	return nil
}

// status renders the card; the caller holds s.mu.
func (s *imageService) status() web.ImageStatus {
	out := web.ImageStatus{Enabled: true, Current: s.current}
	if s.svc == nil {
		out.LayoutError = "reflash once to enable image updates"
	} else {
		live := s.svc.Status()
		out.Ready = live.Ready
		out.Slot = live.Slot
		out.OtherSlot = live.OtherSlot
		out.Bootable = live.Bootable
		out.Switched = live.Switched
		out.CanRollback = live.CanRollback
		out.RollbackTo = live.RollbackTo
		out.LastFailure = live.LastFailure
		if !live.Ready {
			out.LayoutError = "reflash once to enable image updates"
		}
	}
	if img, ok := s.image(s.latest); ok {
		out.Available = s.latest.Version
		out.NotesURL = s.latest.NotesURL
		out.ImageSize = img.Size
		out.Installable = update.IsUpgrade(s.current, s.latest.Version) &&
			s.latest.AllowsUpgradeFrom(s.current) && !img.NeedsLayout()
		if img.NeedsLayout() {
			out.LayoutError = "reflash once to enable image updates"
		}
	}
	return out
}

// image is this board's image in a manifest.
func (s *imageService) image(m *update.Manifest) (imageupdate.Image, bool) {
	if m == nil || s.board == "" {
		return imageupdate.Image{}, false
	}
	a, ok := m.Images[s.board]
	if !ok {
		return imageupdate.Image{}, false
	}
	return imageupdate.Image{File: a.File, SHA256: a.SHA256, Size: a.Size, Layout: a.Layout}, true
}

// assetURL is the manifest URL with its last element replaced by the image
// file, the rule updateService.assetURL follows.
func (s *imageService) assetURL(img imageupdate.Image) string {
	base := s.checker.ManifestURL
	if i := strings.LastIndexByte(base, '/'); i >= 0 {
		base = base[:i]
	}
	return base + "/" + img.File
}

var _ web.ImageUpdater = (*imageService)(nil)
