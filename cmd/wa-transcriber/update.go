// Binary self-updates (M9, D17, ARCHITECTURE.md section 11). This file holds
// everything the running process does with the update directory: the boot
// decision, the health timer, the daily check and the service the System page
// drives. The on-disk state machine, the manifest and the download live in
// internal/update; nothing here execs anything except through execFunc, which
// main supplies (a variable, not a call, so the tests can watch it).
package main

import (
	"context"
	"crypto/ed25519"
	"errors"
	"fmt"
	"log/slog"
	"os"
	"path/filepath"
	"runtime"
	"strings"
	"sync"
	"syscall"
	"time"

	"github.com/emdioh/wa-transcriber/internal/update"
	"github.com/emdioh/wa-transcriber/internal/web"
)

// Timings of the update loop. They are variables so the tests can shrink them;
// on a board they are the numbers of ARCHITECTURE.md section 11.
var (
	// healthyAfter is how long the running version must stay up before it
	// counts as healthy (update.Healthy). Never at start: a process that dies
	// immediately has to be caught by the try gate.
	healthyAfter = 60 * time.Second
	// updateFirstCheck is the quiet period before the first daily check, so
	// a boot is not competing with a download.
	updateFirstCheck = 5 * time.Minute
	// updateInterval is the period of the daily check.
	updateInterval = 24 * time.Hour
)

// updatePubKey is the base64 ed25519 public key, injected at build time with
// -X main.updatePubKey (see the Makefile and release/update-pubkey.txt). An
// empty value compiles self-updates out: no key, no trust, no feature.
var updatePubKey string

// execFunc replaces the process. It is a variable so that main can pass
// syscall.Exec and the tests can record the call instead of losing the process.
var execFunc = syscall.Exec

// bootstrapRequest carries the flags of the bootstrap decision.
type bootstrapRequest struct {
	// UpdateDir is -update-dir: empty means the feature is off (Docker, dev).
	UpdateDir string
	// SelfPath is the running executable (os.Executable()).
	SelfPath string
	// Version is what this binary reports (git describe).
	Version string
	// Skip explains why the decision must not run at all ("-version"), or ""
	// to run it.
	Skip string
}

// bootstrapDecide is the boot decision without the exec: it returns the path
// to exec ("" for "keep running this binary"), a note for the log, whether the
// running binary is the factory one, and Boot's error (already logged by the
// caller, never fatal).
func bootstrapDecide(req bootstrapRequest, now time.Time, pub ed25519.PublicKey) (path, note string, factory bool, err error) {
	switch {
	case req.Skip != "":
		return "", req.Skip + ": bootstrap skipped", false, nil
	case req.UpdateDir == "":
		return "", "off", false, nil
	case len(pub) != ed25519.PublicKeySize:
		return "", "bootstrap without an update key, nothing to verify or exec",
			factoryBinary(req.SelfPath, req.UpdateDir), nil
	}
	factory = factoryBinary(req.SelfPath, req.UpdateDir)
	if !factory {
		// Nothing to decide: the bootstrap already ran at this start, and a
		// second run would count a try or exec the same file again.
		return "", "running from " + req.UpdateDir, false, nil
	}
	act, err := update.Boot(req.UpdateDir, update.Self{
		Version: req.Version,
		Path:    req.SelfPath,
		Factory: factory,
	}, now)
	if act.Note == "" {
		// Boot stays silent when the factory binary simply runs: say so,
		// so the log always shows what the gate decided.
		act.Note = "factory binary, nothing to promote"
	}
	return act.Path, act.Note, factory, err
}

// bootstrapStep runs the decision of bootstrapDecide and execs what it says.
// It returns only when nothing was exec'd: exec replaces the process, so a
// returned error is never fatal — the caller carries on with the binary it is
// already running.
func bootstrapStep(log *slog.Logger, req bootstrapRequest, pub ed25519.PublicKey) {
	path, note, factory, err := bootstrapDecide(req, time.Now(), pub)
	if err != nil {
		// Boot never fails the process; this is the record of a broken state.
		log.Warn("update: boot decision", "err", err)
	}
	if note != "" {
		logDecision(log, note, factory)
	}
	if path == "" {
		return
	}
	log.Info("update: exec", "path", path)
	if err := execFunc(path, os.Args, os.Environ()); err != nil {
		// A kernel that refuses the file (wrong arch, noexec mount) fails
		// here, in the old process: no crash loop, no state to repair.
		log.Warn("update: exec failed, keeping the running binary", "path", path, "err", err)
	}
}

// logDecision logs what the boot decided: a rollback is the one case that
// deserves a warning, everything else is information.
func logDecision(log *slog.Logger, note string, factory bool) {
	if strings.Contains(note, "rollback") {
		log.Warn("update: boot decision", "decision", note, "factory", factory)
		return
	}
	log.Info("update: boot decision", "decision", note, "factory", factory)
}

// selfInDir reports whether the running executable lives in the update
// directory, i.e. the bootstrap already exec'd it.
func selfInDir(selfPath, dir string) bool {
	if selfPath == "" || dir == "" {
		return false
	}
	return sameDir(selfPath, dir)
}

// factoryBinary reports whether the running executable is outside the update
// directory, i.e. the read-only factory binary from /usr/bin.
func factoryBinary(selfPath, dir string) bool {
	return !selfInDir(selfPath, dir)
}

// sameDir reports whether the directory of path is dir. Both arguments are
// file system paths, spelled differently by os.Executable (which may carry a
// resolved symlink) and by the flag, so the comparison walks the parents of
// path instead of assuming one layout.
func sameDir(path, dir string) bool {
	clean := filepath.Clean(path)
	for {
		parent := filepath.Dir(clean)
		if parent == clean {
			return false
		}
		if isSameDirName(parent, dir) {
			return true
		}
		clean = parent
	}
}

// isSameDirName compares two directory names, following symlinks: /data/bin
// and a bind mount or a symlink of it must compare equal.
func isSameDirName(a, b string) bool {
	a, b = filepath.Clean(a), filepath.Clean(b)
	if a == b {
		return true
	}
	fa, err := os.Stat(a)
	if err != nil {
		return false
	}
	fb, err := os.Stat(b)
	if err != nil {
		return false
	}
	return fa.IsDir() && fb.IsDir() && os.SameFile(fa, fb)
}

// bootstrapPath is the path restart() execs: the -bootstrap flag when set,
// otherwise the running executable. A backup binary has to pass the same
// value, because the copied path does not exist (D17).
func bootstrapPath(flagValue string) (string, error) {
	if v := strings.TrimSpace(flagValue); v != "" {
		return v, nil
	}
	return os.Executable()
}

// startUpdateRuntime starts the two background jobs of the feature: the
// health timer after healthyAfter, and the daily check when it is enabled.
// check is nil when there is no update service: then only the health timer
// runs, and that one is harmless (it marks the running version healthy).
func startUpdateRuntime(ctx context.Context, log *slog.Logger, dir string, check *dailyCheck) {
	if dir == "" {
		return
	}
	go func() {
		select {
		case <-ctx.Done():
			return
		case <-time.After(healthyAfter):
		}
		if err := update.Healthy(dir); err != nil {
			log.Warn("update: could not mark the running version healthy", "err", err)
			return
		}
		log.Info("update: running version is healthy", "version", version)
	}()
	if check != nil {
		go check.run(ctx)
	}
}

// updateSettingsSource is the part of *config.Store the daily check reads: the
// effective settings, environment overrides included (D10).
type updateSettingsSource interface {
	UpdateSettings() (checkMode string, auto bool)
}

// dailyCheck is the opt-in daily release check (update_check daily).
type dailyCheck struct {
	svc   *updateService
	log   *slog.Logger
	cfg   updateSettingsSource
	busy  func() bool
	inAP  func() bool
	now   func() time.Time
	first time.Duration
	every time.Duration
	// restart re-execs the bootstrap after an auto install. nil: never
	// install automatically (the daily check only reports).
	restart func()
}

// run checks once after the quiet period, then once per interval, until ctx
// ends. A failed check only logs: the timer keeps running.
func (d *dailyCheck) run(ctx context.Context) {
	t := time.NewTimer(d.first)
	defer t.Stop()
	for {
		select {
		case <-ctx.Done():
			return
		case <-t.C:
		}
		if mode, _ := d.cfg.UpdateSettings(); mode != "daily" {
			// Turned off from the web UI while running: no more network.
			return
		}
		d.tick(ctx)
		t.Reset(d.every)
	}
}

// tick runs one check and, when allowed, the automatic install.
func (d *dailyCheck) tick(ctx context.Context) {
	st, err := d.svc.Check(ctx)
	if err != nil {
		d.log.Warn("update: daily check failed", "err", err)
		return
	}
	if st.Available == "" {
		d.log.Info("update: no new version", "current", st.Current)
		return
	}
	if d.restart == nil || !d.mayInstall() {
		d.log.Info("update: "+st.Available+" is available", "current", st.Current, "notes", st.NotesURL)
		return
	}
	staged, err := d.svc.Install(ctx)
	if err != nil {
		d.log.Warn("update: automatic install failed", "version", st.Available, "err", err)
		return
	}
	d.log.Warn("update: installed, restarting", "version", staged.Installed, "from", st.Current)
	d.restart()
}

// mayInstall reports whether the automatic install may run now: the setting
// is on, the device is not serving the setup AP, and no voice note is queued
// or being transcribed.
func (d *dailyCheck) mayInstall() bool {
	if _, auto := d.cfg.UpdateSettings(); !auto {
		return false
	}
	if d.inAP != nil && d.inAP() {
		d.log.Info("update: setup AP is running, not restarting to install")
		return false
	}
	if d.busy != nil && d.busy() {
		d.log.Info("update: a voice note is queued or running, not restarting to install")
		return false
	}
	return true
}

// updateService is web.Updater over internal/update: it owns the checker, the
// installer and the last verified manifest. One mutex serialises the actions,
// so two clicks cannot interleave two downloads or a download and a rollback.
type updateService struct {
	dir     string
	current string
	checker *update.Checker
	install *update.Installer
	log     *slog.Logger
	now     func() time.Time
	asset   string // "" when this platform has no release asset
	// busy reports a queued or running voice note. Install refuses while it
	// is true and the web UI retries later, so a click never kills a reply.
	busy func() bool

	mu     sync.Mutex
	latest *update.Manifest
	// checkedAt is the last successful check; it survives a restart through
	// the checker's on-disk cache, which is read at construction.
	checkedAt time.Time
}

// newUpdateService builds the service; nil means the feature is off, and nil
// is also what main passes to web.Options so the card stays absent. key is
// the decoded -X main.updatePubKey (nil for a build without one).
func newUpdateService(dir, current, manifestURL, cachePath string, key ed25519.PublicKey,
	busy func() bool, log *slog.Logger) *updateService {
	if dir == "" || len(key) != ed25519.PublicKeySize || manifestURL == "" {
		return nil
	}
	if log == nil {
		log = slog.Default()
	}
	svc := &updateService{
		dir:     dir,
		current: current,
		checker: &update.Checker{ManifestURL: manifestURL, Pub: key, CachePath: cachePath},
		install: &update.Installer{Dir: dir},
		log:     log,
		now:     time.Now,
		busy:    busy,
	}
	if asset := ownAssetKey(); asset != "" {
		svc.asset = asset
	} else {
		log.Warn("update: this architecture has no release asset, checks only", "arch", runtime.GOARCH)
	}
	// A previous run may have left a verified manifest: shows what was
	// available without a request, with the age of that check.
	svc.latest = svc.checker.Cached()
	if svc.latest != nil {
		svc.checkedAt = svc.now()
	}
	return svc
}

// Status implements web.Updater.
func (s *updateService) Status(context.Context) web.UpdateStatus {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.status()
}

// Check implements web.Updater: fetch and verify the newest manifest.
func (s *updateService) Check(ctx context.Context) (web.UpdateStatus, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	if err := s.check(ctx); err != nil {
		return s.status(), err
	}
	return s.status(), nil
}

// Install implements web.Updater: stage Available as the pending slot. The
// caller restarts; the boot gate promotes it.
func (s *updateService) Install(ctx context.Context) (web.UpdateStatus, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.latest == nil || !update.IsUpgrade(s.current, s.latest.Version) {
		if err := s.check(ctx); err != nil {
			return s.status(), err
		}
	}
	m := s.latest
	if m == nil || !update.IsUpgrade(s.current, m.Version) {
		return s.status(), errors.New("no new version to install")
	}
	if !m.AllowsUpgradeFrom(s.current) {
		return s.status(), fmt.Errorf("%s needs at least %s: update in steps", m.Version, m.MinFrom)
	}
	if s.asset == "" {
		return s.status(), errors.New("this architecture has no release asset")
	}
	if s.busy != nil && s.busy() {
		return s.status(), errors.New("a voice note is queued or running, try again in a minute")
	}
	if _, err := s.install.Fetch(ctx, s.assetURL(m), m, s.asset); err != nil {
		return s.status(), err
	}
	s.log.Warn("update: new version staged", "version", m.Version, "from", s.current)
	return s.status(), nil
}

// Rollback implements web.Updater: swap active and previous for the next
// start and restore the matching snapshot. The caller restarts.
func (s *updateService) Rollback(context.Context) (web.UpdateStatus, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	slot, err := update.Rollback(s.dir, s.now())
	if err != nil {
		return s.status(), err
	}
	if slot != nil {
		s.log.Warn("update: rollback staged, restarting", "version", slot.Version)
	}
	return s.status(), nil
}

// check fetches the latest manifest; the caller holds s.mu.
func (s *updateService) check(ctx context.Context) error {
	m, err := s.checker.Latest(ctx)
	if err != nil {
		return err
	}
	s.latest = m
	s.checkedAt = s.now()
	return nil
}

// status renders the current view; the caller holds s.mu.
func (s *updateService) status() web.UpdateStatus {
	st := web.UpdateStatus{Enabled: true, Current: s.current, CheckedAt: s.checkedAt}
	if m := s.latest; m != nil && update.IsUpgrade(s.current, m.Version) && m.AllowsUpgradeFrom(s.current) {
		st.Available = m.Version
		st.NotesURL = m.NotesURL
	}
	state, err := update.LoadState(s.dir)
	if err != nil {
		// A state file that cannot be read is not fatal: the slots below
		// are simply empty and the boot gate repairs what it can.
		s.log.Warn("update: reading the update state", "err", err)
	}
	if state == nil {
		return st
	}
	if state.Pending != nil {
		st.Installed = state.Pending.Version
	}
	if state.Previous != nil {
		st.CanRollback = true
		st.RollbackTo = state.Previous.Version
	}
	if f := state.LastFailure; f != nil {
		st.LastFailure = fmt.Sprintf("%s was rolled back: %s", f.Version, f.Reason)
	}
	return st
}

// assetURL is the manifest URL with its last path element replaced by the
// asset file: .../latest/download/manifest.json → .../latest/download/<file>.
// A test server on a board serves the same layout.
func (s *updateService) assetURL(m *update.Manifest) string {
	a, ok := m.Asset(s.asset)
	if !ok {
		return s.checker.ManifestURL
	}
	base := s.checker.ManifestURL
	if i := strings.LastIndexByte(base, '/'); i >= 0 {
		base = base[:i]
	}
	return base + "/" + a.File
}
