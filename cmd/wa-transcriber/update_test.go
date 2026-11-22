package main

import (
	"context"
	"crypto/ed25519"
	"crypto/rand"
	"encoding/base64"
	"encoding/json"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"runtime"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"github.com/emdioh/wa-transcriber/internal/update"
	"github.com/emdioh/wa-transcriber/internal/web"
)

// updateTestNow is the clock the state machine sees in these tests.
var updateTestNow = time.Date(2026, 11, 22, 12, 0, 0, 0, time.UTC)

// releaseServer is a stand-in for the GitHub "latest release" CDN: it serves
// a signed manifest.json, its signature and one asset, and counts requests.
type releaseServer struct {
	*httptest.Server
	manifest  []byte
	sig       []byte
	asset     []byte
	assetFile string
	// failAsset answers the asset with 500 (a broken download).
	failAsset atomic.Bool
	assetGets atomic.Int32
}

// newReleaseServer signs m with key and serves it under /latest/download/,
// the layout the updater builds its asset URLs from.
func newReleaseServer(t *testing.T, key ed25519.PrivateKey, m *update.Manifest, asset []byte, file string) *releaseServer {
	t.Helper()
	body, err := json.Marshal(m)
	if err != nil {
		t.Fatal(err)
	}
	rs := &releaseServer{manifest: body, sig: ed25519.Sign(key, body), asset: asset, assetFile: file}
	mux := http.NewServeMux()
	mux.HandleFunc("/latest/download/manifest.json", func(w http.ResponseWriter, r *http.Request) {
		_, _ = w.Write(rs.manifest)
	})
	mux.HandleFunc("/latest/download/manifest.json.ed25519", func(w http.ResponseWriter, r *http.Request) {
		_, _ = w.Write([]byte(base64.StdEncoding.EncodeToString(rs.sig) + "\n"))
	})
	mux.HandleFunc("/latest/download/"+file, func(w http.ResponseWriter, r *http.Request) {
		rs.assetGets.Add(1)
		if rs.failAsset.Load() {
			http.Error(w, "boom", http.StatusInternalServerError)
			return
		}
		_, _ = w.Write(rs.asset)
	})
	rs.Server = httptest.NewServer(mux)
	t.Cleanup(rs.Close)
	return rs
}

func (rs *releaseServer) manifestURL() string { return rs.URL + "/latest/download/manifest.json" }

// updateKeys makes a fresh signing keypair for one test.
func updateKeys(t *testing.T) (ed25519.PublicKey, ed25519.PrivateKey) {
	t.Helper()
	pub, priv, err := ed25519.GenerateKey(rand.Reader)
	if err != nil {
		t.Fatal(err)
	}
	return pub, priv
}

// testManifest describes a release of version whose only asset is the running
// test binary: a real ELF for this machine, which CheckExecutable insists on.
func testManifest(t *testing.T, version, key string, body []byte) *update.Manifest {
	t.Helper()
	return &update.Manifest{
		Schema:     1,
		Version:    version,
		ReleasedAt: updateTestNow,
		NotesURL:   "https://github.com/emdioh/wa-transcriber/releases/tag/" + version,
		Assets: map[string]update.Asset{
			key: {File: "wa-transcriber-" + version + "-" + key, SHA256: update.Sum(body), Size: int64(len(body))},
		},
	}
}

// testBinary copies the test binary into dir (a genuine executable, which is
// what the local arch needs) and returns its bytes.
func testBinary(t *testing.T, dir string) []byte {
	t.Helper()
	self, err := os.Executable()
	if err != nil {
		t.Fatal(err)
	}
	b, err := os.ReadFile(self)
	if err != nil {
		t.Fatal(err)
	}
	dst := filepath.Join(dir, "wa-transcriber-test-binary")
	if err := os.WriteFile(dst, b, 0o755); err != nil {
		t.Fatal(err)
	}
	return b
}

// newTestService builds a service over dir, with the URL of rs and key.
func newTestService(dir, current string, rs *releaseServer, key ed25519.PublicKey, busy func() bool) *updateService {
	svc := newUpdateService(dir, current, rs.manifestURL(),
		filepath.Join(filepath.Dir(dir), "updates", "check.json"), key, busy,
		slog.New(slog.NewTextHandler(os.Stderr, &slog.HandlerOptions{Level: slog.LevelError})))
	if svc != nil {
		svc.now = func() time.Time { return updateTestNow }
	}
	return svc
}

func TestUpdateServiceDisabled(t *testing.T) {
	_, priv := updateKeys(t)
	rs := newReleaseServer(t, priv, testManifest(t, "v9.0.0", "linux-amd64", nil), nil, "x")
	if svc := newTestService(t.TempDir(), "v0.1.0", rs, nil, nil); svc != nil {
		t.Fatal("a build without a public key must be disabled")
	}
	// An empty key decodes as nil without an error.
	if svc := newTestService(t.TempDir(), "v0.1.0", rs, []byte{}, nil); svc != nil {
		t.Fatal("an empty public key must be disabled")
	}
	pub, _ := updateKeys(t)
	if svc := newTestService("", "v0.1.0", rs, pub, nil); svc != nil {
		t.Fatal("an empty update dir must be disabled")
	}
}

func TestUpdateServiceLifecycle(t *testing.T) {
	dir := t.TempDir()
	binDir := filepath.Join(dir, "bin") // /data/bin: the snapshot lives in /data/rollback
	body := testBinary(t, dir)
	asset := ownAssetKey()
	if asset == "" {
		t.Skipf("no release asset for %s", runtime.GOARCH)
	}

	// A device already running an older release: active=v0.1.0,
	// previous=v0.0.9, so both "install" and "roll back" have a target.
	previous := filepath.Join(binDir, "wa-transcriber-v0.0.9")
	if err := os.MkdirAll(binDir, 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(previous, body, 0o755); err != nil {
		t.Fatal(err)
	}
	active := filepath.Join(binDir, "wa-transcriber-v0.1.0")
	if err := os.WriteFile(active, body, 0o755); err != nil {
		t.Fatal(err)
	}
	st, err := update.LoadState(binDir)
	if err != nil {
		t.Fatal(err)
	}
	st.Active = &update.Slot{Version: "v0.1.0", File: active, SHA256: update.Sum(body)}
	st.Previous = &update.Slot{Version: "v0.0.9", File: previous, SHA256: update.Sum(body)}
	st.Verified = true
	if err := update.SaveState(binDir, st); err != nil {
		t.Fatal(err)
	}

	pub, priv := updateKeys(t)
	m := testManifest(t, "v0.2.0", asset, body)
	rs := newReleaseServer(t, priv, m, body, m.Assets[asset].File)

	var busy atomic.Bool
	svc := newTestService(binDir, "v0.1.0", rs, pub, busy.Load)
	if svc == nil {
		t.Fatal("service disabled")
	}
	ctx := context.Background()

	// Before any check: nothing available, but the slots are known.
	s := svc.Status(ctx)
	if !s.Enabled || s.Current != "v0.1.0" {
		t.Fatalf("status = %+v", s)
	}
	if s.Available != "" || !s.CheckedAt.IsZero() {
		t.Fatalf("a status without a check names a release: %+v", s)
	}
	if !s.CanRollback || s.RollbackTo != "v0.0.9" || s.Installed != "" {
		t.Fatalf("slots = %+v", s)
	}

	// Check: v0.2.0 appears with its notes.
	s, err = svc.Check(ctx)
	if err != nil {
		t.Fatalf("check: %v", err)
	}
	if s.Available != "v0.2.0" || s.NotesURL != m.NotesURL {
		t.Fatalf("after check: %+v", s)
	}
	if !s.CheckedAt.Equal(updateTestNow) {
		t.Fatalf("CheckedAt = %v", s.CheckedAt)
	}

	// A queued note blocks the install (a restart would kill its reply).
	busy.Store(true)
	if _, err := svc.Install(ctx); err == nil || !strings.Contains(err.Error(), "voice note") {
		t.Fatalf("install while busy: err = %v", err)
	}
	busy.Store(false)

	// Install: the asset is staged as pending, nothing runs it yet.
	s, err = svc.Install(ctx)
	if err != nil {
		t.Fatalf("install: %v", err)
	}
	if s.Installed != "v0.2.0" {
		t.Fatalf("installed = %q", s.Installed)
	}
	state, err := update.LoadState(binDir)
	if err != nil {
		t.Fatal(err)
	}
	if state.Pending == nil || state.Pending.Version != "v0.2.0" {
		t.Fatalf("pending = %+v", state.Pending)
	}
	if _, err := os.Stat(state.Pending.File); err != nil {
		t.Fatalf("pending file: %v", err)
	}
	// The active slot was not touched by the install.
	if state.Active == nil || state.Active.Version != "v0.1.0" {
		t.Fatalf("active = %+v", state.Active)
	}

	// Rollback: active and previous swap, pending is dropped.
	s, err = svc.Rollback(ctx)
	if err != nil {
		t.Fatalf("rollback: %v", err)
	}
	if s.Current != "v0.1.0" || s.Installed != "" {
		t.Fatalf("after rollback: %+v", s)
	}
	// update.Rollback swaps active and previous, so the version it left is
	// the next rollback target (a second click goes forward again).
	if !s.CanRollback || s.RollbackTo != "v0.1.0" {
		t.Fatalf("rollback target = %q (can %v), want the version it left", s.RollbackTo, s.CanRollback)
	}
	if s.LastFailure == "" {
		t.Fatal("a manual rollback should be reported as the last failure")
	}
	state, err = update.LoadState(binDir)
	if err != nil {
		t.Fatal(err)
	}
	if state.Active == nil || state.Active.Version != "v0.0.9" {
		t.Fatalf("active after rollback = %+v", state.Active)
	}
	if state.Pending != nil {
		t.Fatalf("pending after rollback = %+v", state.Pending)
	}
	if _, err := os.Stat(filepath.Join(dir, "updates")); err != nil && !os.IsNotExist(err) {
		t.Fatalf("updates dir: %v", err)
	}
}

func TestUpdateServiceNoNewVersion(t *testing.T) {
	dir := t.TempDir()
	body := testBinary(t, dir)
	asset := ownAssetKey()
	if asset == "" {
		t.Skip("no release asset for this arch")
	}
	pub, priv := updateKeys(t)
	// The running version is the newest release (and a dev build is newer
	// than everything): no Available either way.
	rs := newReleaseServer(t, priv, testManifest(t, "v0.2.0", asset, body), body, "asset")
	binDir := filepath.Join(dir, "bin")
	for _, current := range []string{"v0.2.0", "dev"} {
		svc := newTestService(binDir, current, rs, pub, nil)
		if svc == nil {
			t.Fatal("service disabled")
		}
		s, err := svc.Check(context.Background())
		if err != nil {
			t.Fatalf("%s: check: %v", current, err)
		}
		if s.Available != "" {
			t.Fatalf("%s: Available = %q", current, s.Available)
		}
		if _, err := svc.Install(context.Background()); err == nil {
			t.Fatalf("%s: install without a new version", current)
		}
	}
	if rs.assetGets.Load() != 0 {
		t.Fatalf("the asset was downloaded %d times", rs.assetGets.Load())
	}
}

func TestUpdateServiceFailedDownloadLeavesNoPending(t *testing.T) {
	dir := t.TempDir()
	body := testBinary(t, dir)
	asset := ownAssetKey()
	if asset == "" {
		t.Skip("no release asset for this arch")
	}
	pub, priv := updateKeys(t)
	m := testManifest(t, "v0.3.0", asset, body)
	rs := newReleaseServer(t, priv, m, body, m.Assets[asset].File)
	rs.failAsset.Store(true)
	binDir := filepath.Join(dir, "bin")
	svc := newTestService(binDir, "v0.1.0", rs, pub, nil)
	if svc == nil {
		t.Fatal("service disabled")
	}
	ctx := context.Background()
	s, err := svc.Check(ctx)
	if err != nil || s.Available != "v0.3.0" {
		t.Fatalf("check: %+v, %v", s, err)
	}
	if _, err := svc.Install(ctx); err == nil {
		t.Fatal("a 500 on the asset installed something")
	}
	s = svc.Status(ctx)
	if s.Installed != "" {
		t.Fatalf("Installed = %q after a failed download", s.Installed)
	}
	state, err := update.LoadState(binDir)
	if err != nil {
		t.Fatal(err)
	}
	if state.Pending != nil || state.Active != nil {
		t.Fatalf("state after a failed download = %+v", state)
	}
	// No .part survives, and no binary was written into the slot dir.
	if parts, _ := filepath.Glob(filepath.Join(dir, "updates", "*.part")); len(parts) != 0 {
		t.Fatalf(".part files left: %v", parts)
	}
	if bins, _ := filepath.Glob(filepath.Join(binDir, "wa-transcriber-v*")); len(bins) != 0 {
		t.Fatalf("binaries left after a failed download: %v", bins)
	}
}

func TestUpdateServiceRefusesBadSignature(t *testing.T) {
	dir := t.TempDir()
	body := testBinary(t, dir)
	asset := ownAssetKey()
	if asset == "" {
		t.Skip("no release asset for this arch")
	}
	_, signing := updateKeys(t)
	pub, _ := updateKeys(t) // a different key than the one signing
	rs := newReleaseServer(t, signing, testManifest(t, "v0.4.0", asset, body), body, "asset")
	svc := newTestService(filepath.Join(dir, "bin"), "v0.1.0", rs, pub, nil)
	if svc == nil {
		t.Fatal("service disabled")
	}
	if _, err := svc.Check(context.Background()); err == nil {
		t.Fatal("a manifest signed by another key was accepted")
	}
}

func TestUpdateServiceAssetURL(t *testing.T) {
	pub, _ := updateKeys(t)
	svc := newUpdateService("/data/bin", "v0.1.0", "https://example.test/a/latest/download/manifest.json",
		"", pub, nil, slog.New(slog.NewTextHandler(os.Stderr, nil)))
	if svc == nil {
		t.Fatal("service disabled")
	}
	svc.asset = "linux-armv7"
	m := &update.Manifest{Assets: map[string]update.Asset{
		"linux-armv7": {File: "wa-transcriber-linux-armv7"},
	}}
	if got, want := svc.assetURL(m), "https://example.test/a/latest/download/wa-transcriber-linux-armv7"; got != want {
		t.Fatalf("asset URL = %q, want %q", got, want)
	}
	// An asset the release does not carry falls back to the manifest URL and
	// Fetch refuses it: no install without a matching file.
	if got := svc.assetURL(&update.Manifest{Assets: map[string]update.Asset{"linux-amd64": {File: "x"}}}); got == "" {
		t.Fatal("empty asset URL")
	}
}

func TestUpdateServiceImplementsUpdater(t *testing.T) {
	var _ web.Updater = (*updateService)(nil)
}
