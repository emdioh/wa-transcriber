package main

import (
	"context"
	"crypto/ed25519"
	"errors"
	"io"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/emdioh/wa-transcriber/internal/update"
)

// testKey is a decoded public key for the decisions that want one.
func testKey(t *testing.T) ed25519.PublicKey {
	t.Helper()
	pub, _ := updateKeys(t)
	return pub
}

// slots lays out a /data-like tree: dir/bin is the update directory and
// factory is a binary outside it. It returns the two paths.
func slots(t *testing.T) (dir, bin, factory string) {
	t.Helper()
	dir = t.TempDir()
	bin = filepath.Join(dir, "bin")
	if err := os.MkdirAll(bin, 0o755); err != nil {
		t.Fatal(err)
	}
	factory = filepath.Join(dir, "usr", "bin", "wa-transcriber")
	if err := os.MkdirAll(filepath.Dir(factory), 0o755); err != nil {
		t.Fatal(err)
	}
	self, err := os.Executable()
	if err != nil {
		t.Fatal(err)
	}
	body, err := os.ReadFile(self)
	if err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(factory, body, 0o755); err != nil {
		t.Fatal(err)
	}
	return dir, bin, factory
}

func TestBootstrapDecideOff(t *testing.T) {
	// No update dir (Docker, dev machines): nothing is read, nothing execs.
	path, note, factory, err := bootstrapDecide(bootstrapRequest{SelfPath: "/usr/bin/wa-transcriber", Version: "v0.1.0"},
		updateTestNow, nil)
	if path != "" || err != nil || factory {
		t.Fatalf("path=%q note=%q factory=%v err=%v", path, note, factory, err)
	}
	if note == "" {
		t.Fatal("the decision should say that the feature is off")
	}
}

func TestBootstrapDecideSkipsVersionAndHealthcheck(t *testing.T) {
	dir, bin, factory := slots(t)
	// A pending version would be promoted by a real boot; -version must not
	// touch it.
	body := testBinary(t, dir)
	pending := filepath.Join(bin, "wa-transcriber-v0.9.0")
	if err := os.WriteFile(pending, body, 0o755); err != nil {
		t.Fatal(err)
	}
	st, _ := update.LoadState(bin)
	st.Pending = &update.Slot{Version: "v0.9.0", File: pending, SHA256: update.Sum(body)}
	if err := update.SaveState(bin, st); err != nil {
		t.Fatal(err)
	}
	for _, skip := range []string{"-version", "-healthcheck"} {
		path, _, _, err := bootstrapDecide(bootstrapRequest{UpdateDir: bin, SelfPath: factory,
			Version: "v0.1.0", Skip: skip}, updateTestNow, testKey(t))
		if path != "" || err != nil {
			t.Fatalf("%s: path=%q err=%v", skip, path, err)
		}
	}
	st, _ = update.LoadState(bin)
	if st.Pending == nil || st.Active != nil {
		t.Fatalf("the skip touched the state: %+v", st)
	}
}

func TestBootstrapDecideFreshCard(t *testing.T) {
	_, bin, factory := slots(t)
	path, note, isFactory, err := bootstrapDecide(bootstrapRequest{UpdateDir: bin, SelfPath: factory,
		Version: "v0.1.0"}, updateTestNow, testKey(t))
	if err != nil {
		t.Fatalf("boot: %v", err)
	}
	if path != "" {
		t.Fatalf("a fresh card execs %q", path)
	}
	if !isFactory {
		t.Fatal("a binary outside the update dir is the factory one")
	}
	// A fresh card has nothing to say beyond "the factory binary runs".
	if note == "" {
		t.Fatal("the decision logged nothing")
	}
}

func TestBootstrapDecidePromotesPending(t *testing.T) {
	dir, bin, factory := slots(t)
	body := testBinary(t, dir)
	pending := filepath.Join(bin, "wa-transcriber-v0.2.0")
	if err := os.WriteFile(pending, body, 0o755); err != nil {
		t.Fatal(err)
	}
	st, _ := update.LoadState(bin)
	st.Pending = &update.Slot{Version: "v0.2.0", File: pending, SHA256: update.Sum(body)}
	if err := update.SaveState(bin, st); err != nil {
		t.Fatal(err)
	}
	path, note, isFactory, err := bootstrapDecide(bootstrapRequest{UpdateDir: bin, SelfPath: factory,
		Version: "v0.1.0"}, updateTestNow, testKey(t))
	if err != nil {
		t.Fatalf("boot: %v", err)
	}
	if path != pending {
		t.Fatalf("exec %q, want the promoted pending slot %q", path, pending)
	}
	if !strings.Contains(note, "promote") || !strings.Contains(note, "v0.2.0") {
		t.Fatalf("note = %q", note)
	}
	if !isFactory {
		t.Fatal("the factory binary ran the gate")
	}
}

func TestBootstrapDecideRollsBackOnce(t *testing.T) {
	_, bin, factory := slots(t)
	// An active slot whose file is gone: Boot rolls back to nothing, records
	// the failure and lets the factory binary run.
	st, _ := update.LoadState(bin)
	st.Active = &update.Slot{Version: "v0.3.0", File: filepath.Join(bin, "wa-transcriber-v0.3.0"), SHA256: update.Sum([]byte("x"))}
	if err := update.SaveState(bin, st); err != nil {
		t.Fatal(err)
	}
	path, note, _, err := bootstrapDecide(bootstrapRequest{UpdateDir: bin, SelfPath: factory,
		Version: "v0.1.0"}, updateTestNow, testKey(t))
	if err != nil {
		t.Fatalf("boot: %v", err)
	}
	if path != "" {
		t.Fatalf("exec %q after a rollback without a previous version", path)
	}
	if !strings.Contains(note, "rollback") {
		t.Fatalf("note = %q", note)
	}
	after, err := update.LoadState(bin)
	if err != nil {
		t.Fatal(err)
	}
	if after.LastFailure == nil || after.LastFailure.Version != "v0.3.0" {
		t.Fatalf("last failure = %+v", after.LastFailure)
	}
}

func TestBootstrapDecideAlreadyRunningFromData(t *testing.T) {
	dir, bin, _ := slots(t)
	// The factory binary exec'd /data/bin/wa-transcriber-v0.2.0: the second
	// run of the gate in the same start must not count a try or exec again.
	self := filepath.Join(bin, "wa-transcriber-v0.2.0")
	body := testBinary(t, dir)
	if err := os.WriteFile(self, body, 0o755); err != nil {
		t.Fatal(err)
	}
	path, note, isFactory, err := bootstrapDecide(bootstrapRequest{UpdateDir: bin, SelfPath: self,
		Version: "v0.2.0"}, updateTestNow, testKey(t))
	if err != nil || path != "" {
		t.Fatalf("path=%q err=%v", path, err)
	}
	if isFactory {
		t.Fatal("a binary inside the update dir is not the factory one")
	}
	if !strings.Contains(note, "running from") {
		t.Fatalf("note = %q", note)
	}
	if st, _ := update.LoadState(bin); st.Active != nil || st.Tries != 0 {
		t.Fatalf("the decision touched the state: %+v", st)
	}
}

func TestBootstrapDecideWithoutKey(t *testing.T) {
	dir, bin, factory := slots(t)
	body := testBinary(t, dir)
	pending := filepath.Join(bin, "wa-transcriber-v0.2.0")
	if err := os.WriteFile(pending, body, 0o755); err != nil {
		t.Fatal(err)
	}
	st, _ := update.LoadState(bin)
	st.Pending = &update.Slot{Version: "v0.2.0", File: pending, SHA256: update.Sum(body)}
	if err := update.SaveState(bin, st); err != nil {
		t.Fatal(err)
	}
	// A build without a key trusts nothing, so it execs nothing: the factory
	// binary keeps running and the pending file stays for a signed build.
	path, _, _, err := bootstrapDecide(bootstrapRequest{UpdateDir: bin, SelfPath: factory,
		Version: "v0.1.0"}, updateTestNow, nil)
	if err != nil || path != "" {
		t.Fatalf("path=%q err=%v", path, err)
	}
	if st, _ := update.LoadState(bin); st.Pending == nil {
		t.Fatal("the pending slot was dropped by a binary without a key")
	}
}

func TestBootstrapPath(t *testing.T) {
	if got, err := bootstrapPath("/usr/bin/wa-transcriber"); err != nil || got != "/usr/bin/wa-transcriber" {
		t.Fatalf("flag = %q, %v", got, err)
	}
	if got, err := bootstrapPath(""); err != nil || got == "" {
		t.Fatalf("default = %q, %v", got, err)
	}
}

// fakeExec replaces execFunc for one test and records the calls.
func fakeExec(t *testing.T, err error) *[][]string {
	t.Helper()
	var calls [][]string
	old := execFunc
	execFunc = func(path string, argv, env []string) error {
		calls = append(calls, append([]string{path}, argv...))
		return err
	}
	t.Cleanup(func() { execFunc = old })
	return &calls
}

func TestBootstrapStepExecs(t *testing.T) {
	dir, bin, factory := slots(t)
	body := testBinary(t, dir)
	pending := filepath.Join(bin, "wa-transcriber-v0.2.0")
	if err := os.WriteFile(pending, body, 0o755); err != nil {
		t.Fatal(err)
	}
	st, _ := update.LoadState(bin)
	st.Pending = &update.Slot{Version: "v0.2.0", File: pending, SHA256: update.Sum(body)}
	if err := update.SaveState(bin, st); err != nil {
		t.Fatal(err)
	}
	calls := fakeExec(t, nil)
	bootstrapStep(slog.New(slog.NewTextHandler(io.Discard, nil)), bootstrapRequest{UpdateDir: bin,
		SelfPath: factory, Version: "v0.1.0"}, testKey(t))
	if len(*calls) != 1 || (*calls)[0][0] != pending {
		t.Fatalf("exec calls = %v", *calls)
	}
}

func TestBootstrapStepSurvivesExecFailure(t *testing.T) {
	dir, bin, factory := slots(t)
	body := testBinary(t, dir)
	pending := filepath.Join(bin, "wa-transcriber-v0.2.0")
	if err := os.WriteFile(pending, body, 0o755); err != nil {
		t.Fatal(err)
	}
	st, _ := update.LoadState(bin)
	st.Pending = &update.Slot{Version: "v0.2.0", File: pending, SHA256: update.Sum(body)}
	if err := update.SaveState(bin, st); err != nil {
		t.Fatal(err)
	}
	calls := fakeExec(t, errors.New("exec format error"))
	var buf strings.Builder
	log := slog.New(slog.NewTextHandler(&buf, nil))
	// A refused exec must return, not panic: the old binary keeps running.
	bootstrapStep(log, bootstrapRequest{UpdateDir: bin, SelfPath: factory, Version: "v0.1.0"}, testKey(t))
	if len(*calls) != 1 {
		t.Fatalf("exec calls = %v", *calls)
	}
	if !strings.Contains(buf.String(), "exec failed") {
		t.Fatalf("log = %s", buf.String())
	}
}

// fakeSettings is the config store the daily check reads.
type fakeSettings struct {
	mu     sync.Mutex
	mode   string
	auto   bool
	setGet int
}

func (f *fakeSettings) UpdateSettings() (string, bool) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.setGet++
	return f.mode, f.auto
}

func (f *fakeSettings) set(mode string, auto bool) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.mode, f.auto = mode, auto
}

// dailyFixture is a service whose server offers one newer release.
func dailyFixture(t *testing.T, current string, busy, inAP *atomic.Bool) (*dailyCheck, *releaseServer, *fakeSettings) {
	t.Helper()
	dir := t.TempDir()
	body := testBinary(t, dir)
	asset := ownAssetKey()
	if asset == "" {
		t.Skip("no release asset for this arch")
	}
	pub, priv := updateKeys(t)
	m := testManifest(t, "v0.5.0", asset, body)
	// testManifest names its asset after the version; the server serves that.
	rs := newReleaseServer(t, priv, m, body, m.Assets[asset].File)
	svc := newTestService(filepath.Join(dir, "bin"), current, rs, pub, nil)
	if svc == nil {
		t.Fatal("service disabled")
	}
	cfg := &fakeSettings{mode: "daily"}
	d := &dailyCheck{
		svc: svc,
		log: slog.New(slog.NewTextHandler(io.Discard, nil)),
		cfg: cfg,
		now: time.Now,
	}
	if busy != nil {
		d.busy = busy.Load
	}
	if inAP != nil {
		d.inAP = inAP.Load
	}
	return d, rs, cfg
}

// failingServer answers every request with 500: a check that cannot succeed.
func failingServer(t *testing.T) string {
	t.Helper()
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		http.Error(w, "down", http.StatusInternalServerError)
	}))
	t.Cleanup(srv.Close)
	return srv.URL
}

func TestDailyCheckReportsOnly(t *testing.T) {
	d, rs, cfg := dailyFixture(t, "v0.1.0", nil, nil)
	d.tick(context.Background())
	if rs.assetGets.Load() != 0 {
		t.Fatalf("auto off: the asset was downloaded %d times", rs.assetGets.Load())
	}
	if d.svc.Status(context.Background()).Available != "v0.5.0" {
		t.Fatal("the check did not find the release")
	}
	cfg.set("daily", true)
	if d.mayInstall() != true {
		t.Fatal("auto on, nothing busy: may install")
	}
	cfg.set("off", false)
	if d.mayInstall() {
		t.Fatal("auto off: may not install")
	}
}

func TestDailyCheckStaysOutOfTheWay(t *testing.T) {
	var busy, inAP atomic.Bool
	d, rs, _ := dailyFixture(t, "v0.1.0", &busy, &inAP)
	restarted := atomic.Int32{}
	d.restart = func() { restarted.Add(1) }
	d.cfg.(*fakeSettings).set("daily", true)

	inAP.Store(true)
	d.tick(context.Background())
	if restarted.Load() != 0 || rs.assetGets.Load() != 0 {
		t.Fatal("installed while the setup AP was running")
	}
	inAP.Store(false)

	busy.Store(true)
	d.tick(context.Background())
	if restarted.Load() != 0 || rs.assetGets.Load() != 0 {
		t.Fatal("installed while a voice note was queued")
	}
	busy.Store(false)

	// Nothing in the way: fetch, stage and ask for the restart.
	d.tick(context.Background())
	if rs.assetGets.Load() != 1 {
		t.Fatalf("asset downloads = %d, want 1", rs.assetGets.Load())
	}
	if restarted.Load() != 1 {
		t.Fatalf("restarts = %d, want 1", restarted.Load())
	}
	if d.svc.Status(context.Background()).Installed != "v0.5.0" {
		t.Fatal("nothing staged")
	}
}

func TestDailyCheckFailureKeepsTheTimer(t *testing.T) {
	dir := t.TempDir()
	// A server that answers 500 on the manifest: the check fails, the loop
	// must still run its later ticks.
	srv := failingServer(t)
	pub, _ := updateKeys(t)
	svc := newUpdateService(filepath.Join(dir, "bin"), "v0.1.0", srv+"/manifest.json", "", pub, nil,
		slog.New(slog.NewTextHandler(io.Discard, nil)))
	if svc == nil {
		t.Fatal("service disabled")
	}
	cfg := &fakeSettings{mode: "daily", auto: true}
	restarts := atomic.Int32{}
	d := &dailyCheck{
		svc:     svc,
		log:     slog.New(slog.NewTextHandler(io.Discard, nil)),
		cfg:     cfg,
		first:   time.Millisecond,
		every:   time.Millisecond,
		restart: func() { restarts.Add(1) },
	}
	ctx, cancel := context.WithTimeout(context.Background(), 200*time.Millisecond)
	defer cancel()
	d.run(ctx)
	if restarts.Load() != 0 {
		t.Fatal("restarted without a verified manifest")
	}
	if cfg.setGet == 0 {
		t.Fatal("the loop never ran a tick")
	}
}

func TestDailyCheckStopsWhenTurnedOff(t *testing.T) {
	dir := t.TempDir()
	srv := failingServer(t)
	pub, _ := updateKeys(t)
	svc := newUpdateService(filepath.Join(dir, "bin"), "v0.1.0", srv+"/manifest.json", "", pub, nil,
		slog.New(slog.NewTextHandler(io.Discard, nil)))
	if svc == nil {
		t.Fatal("service disabled")
	}
	cfg := &fakeSettings{mode: "off"}
	d := &dailyCheck{svc: svc, log: slog.New(slog.NewTextHandler(io.Discard, nil)), cfg: cfg,
		first: time.Millisecond, every: time.Millisecond}
	done := make(chan struct{})
	go func() { d.run(context.Background()); close(done) }()
	select {
	case <-done:
	case <-time.After(2 * time.Second):
		t.Fatal("run did not stop with the setting off")
	}
	if cfg.setGet == 0 {
		t.Fatal("run never read the setting")
	}
}

func TestStartUpdateRuntimeInert(t *testing.T) {
	// No update dir: no timer, no state, nothing to wait for.
	startUpdateRuntime(context.Background(), slog.New(slog.NewTextHandler(io.Discard, nil)), "", nil)
}

func TestStartUpdateRuntimeMarksHealthy(t *testing.T) {
	dir := t.TempDir()
	body := testBinary(t, dir)
	bin := filepath.Join(dir, "bin")
	if err := os.MkdirAll(bin, 0o755); err != nil {
		t.Fatal(err)
	}
	self := filepath.Join(bin, "wa-transcriber-v0.2.0")
	if err := os.WriteFile(self, body, 0o755); err != nil {
		t.Fatal(err)
	}
	st, _ := update.LoadState(bin)
	st.Active = &update.Slot{Version: "v0.2.0", File: self, SHA256: update.Sum(body)}
	st.Tries = 2
	if err := update.SaveState(bin, st); err != nil {
		t.Fatal(err)
	}
	old := healthyAfter
	healthyAfter = 10 * time.Millisecond
	t.Cleanup(func() { healthyAfter = old })
	startUpdateRuntime(context.Background(), slog.New(slog.NewTextHandler(io.Discard, nil)), bin, nil)

	deadline := time.Now().Add(2 * time.Second)
	for time.Now().Before(deadline) {
		if s, _ := update.LoadState(bin); s.Verified && s.Tries == 0 {
			return
		}
		time.Sleep(5 * time.Millisecond)
	}
	t.Fatal("the running version was never marked healthy")
}
