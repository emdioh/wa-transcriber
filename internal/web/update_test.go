package web

import (
	"context"
	"errors"
	"net/http"
	"net/http/httptest"
	"net/url"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/emdioh/wa-transcriber/internal/config"
	"github.com/emdioh/wa-transcriber/internal/logbuf"
	"github.com/emdioh/wa-transcriber/internal/status"
)

// fakeUpdater records the calls and returns st (or the configured error).
type fakeUpdater struct {
	mu       sync.Mutex
	st       UpdateStatus
	checkErr error
	instErr  error
	rollErr  error
	checks   int
	installs int
	rolls    int
}

func (f *fakeUpdater) get() UpdateStatus {
	f.mu.Lock()
	defer f.mu.Unlock()
	return f.st
}

func (f *fakeUpdater) Status(context.Context) UpdateStatus { return f.get() }

func (f *fakeUpdater) Check(context.Context) (UpdateStatus, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.checks++
	f.st.CheckedAt = time.Now()
	return f.st, f.checkErr
}

func (f *fakeUpdater) Install(context.Context) (UpdateStatus, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.installs++
	f.st.Installed = f.st.Available
	return f.st, f.instErr
}

func (f *fakeUpdater) Rollback(context.Context) (UpdateStatus, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.rolls++
	return f.st, f.rollErr
}

func (f *fakeUpdater) counts() (checks, installs, rolls int) {
	f.mu.Lock()
	defer f.mu.Unlock()
	return f.checks, f.installs, f.rolls
}

// updateHarness is newHarness plus an updater and a Restart counter.
type updateHarness struct {
	*harness
	up      *fakeUpdater
	rMu     sync.Mutex
	restart int
}

func newUpdateHarness(t *testing.T, st UpdateStatus) *updateHarness {
	t.Helper()
	cfg, err := config.Open(filepath.Join(t.TempDir(), "config.json"))
	if err != nil {
		t.Fatal(err)
	}
	h := &harness{t: t, cfg: cfg, st: status.New(time.Now()), wa: &fakeWA{},
		logs: logbuf.New(1<<16, nil), env: map[string]string{}}
	u := &updateHarness{harness: h, up: &fakeUpdater{st: st}}
	s := New(Options{
		Config: cfg, Status: h.st, Logs: h.logs, WA: h.wa,
		Getenv:  func(k string) string { return h.env[k] },
		Addrs:   func() []status.Addr { return nil },
		Version: "test",
		Updater: u.up,
		Restart: func() { u.rMu.Lock(); u.restart++; u.rMu.Unlock() },
	})
	h.srv = httptest.NewServer(s.Handler())
	t.Cleanup(h.srv.Close)
	h.c = h.newClient()
	h.setup()
	return u
}

func (u *updateHarness) restarts() int {
	u.rMu.Lock()
	defer u.rMu.Unlock()
	return u.restart
}

func TestUpdateCardHiddenWithoutUpdater(t *testing.T) {
	h := newHarness(t)
	h.setup()
	_, _, body := h.get(h.c, "/system")
	if strings.Contains(body, "Software") || strings.Contains(body, "Check for updates") {
		t.Fatal("the update card is rendered without an updater")
	}
	for _, p := range []string{"/system/update/check", "/system/update/install", "/system/update/rollback"} {
		code, _, _ := h.post(h.c, p, url.Values{"confirm": {"yes"}})
		if code != http.StatusNotFound {
			t.Fatalf("%s without an updater: %d, want 404", p, code)
		}
	}
}

func TestUpdateCardHiddenWhenDisabled(t *testing.T) {
	u := newUpdateHarness(t, UpdateStatus{Enabled: false, Current: "v0.1.0"})
	_, _, body := u.get(u.c, "/system")
	if strings.Contains(body, "Software") {
		t.Fatal("a disabled updater still renders the card")
	}
	if code, _, _ := u.post(u.c, "/system/update/check", url.Values{}); code != http.StatusNotFound {
		t.Fatalf("check while disabled: %d", code)
	}
}

func TestUpdateCardShowsState(t *testing.T) {
	u := newUpdateHarness(t, UpdateStatus{
		Enabled: true, Current: "v0.1.0", CheckedAt: time.Now().Add(-2 * time.Hour),
	})
	_, _, body := u.get(u.c, "/system")
	for _, want := range []string{"Software", "v0.1.0", "checked 2h0m0s ago", "Check for updates"} {
		if !strings.Contains(body, want) {
			t.Fatalf("card missing %q:\n%s", want, body)
		}
	}
	// Nothing available: no install form and no rollback form.
	for _, unwanted := range []string{"/system/update/install", "/system/update/rollback"} {
		if strings.Contains(body, unwanted) {
			t.Fatalf("card shows %q without an update:\n%s", unwanted, body)
		}
	}

	u.up.mu.Lock()
	u.up.st.Available = "v0.3.0"
	u.up.st.NotesURL = "https://example.org/releases/tag/v0.3.0"
	u.up.st.CanRollback = true
	u.up.st.RollbackTo = "v0.1.1"
	u.up.st.LastFailure = "v0.2.0 was rolled back: 3 starts without becoming healthy"
	u.up.mu.Unlock()

	_, _, body = u.get(u.c, "/system")
	for _, want := range []string{
		"v0.3.0", `href="https://example.org/releases/tag/v0.3.0"`, "release notes",
		`action="/system/update/install"`, `name="confirm" value="yes"`, "Install and restart",
		"reload this page in about 10 seconds", "voice note being transcribed right now loses its reply",
		`action="/system/update/rollback"`, "Back to v0.1.1",
		"v0.2.0 was rolled back: 3 starts without becoming healthy",
	} {
		if !strings.Contains(body, want) {
			t.Fatalf("card missing %q:\n%s", want, body)
		}
	}
}

func TestUpdateCheckResults(t *testing.T) {
	u := newUpdateHarness(t, UpdateStatus{Enabled: true, Current: "v0.1.0"})
	_, path, body := u.post(u.c, "/system/update/check", url.Values{})
	if path != "/system" || !strings.Contains(body, "No new version") {
		t.Fatalf("check without update: %s\n%s", path, body)
	}
	if !strings.Contains(body, "Last check") {
		t.Fatal("the card does not show when the check ran")
	}

	u.up.mu.Lock()
	u.up.st.Available = "v0.3.0"
	u.up.mu.Unlock()
	_, _, body = u.post(u.c, "/system/update/check", url.Values{})
	if !strings.Contains(body, "v0.3.0 is available") {
		t.Fatalf("check with update: %s", body)
	}

	u.up.mu.Lock()
	u.up.checkErr = errors.New("manifest signature does not match")
	u.up.mu.Unlock()
	_, _, body = u.post(u.c, "/system/update/check", url.Values{})
	if !strings.Contains(body, "Could not check for updates") || !strings.Contains(body, "signature") {
		t.Fatalf("check error not reported: %s", body)
	}
	if checks, _, _ := u.up.counts(); checks != 3 {
		t.Fatalf("check calls = %d, want 3", checks)
	}
	if u.restarts() != 0 {
		t.Fatal("a check restarted the device")
	}
}

func TestUpdateInstallNeedsConfirmation(t *testing.T) {
	u := newUpdateHarness(t, UpdateStatus{Enabled: true, Current: "v0.1.0", Available: "v0.3.0"})
	for _, form := range []url.Values{{}, {"confirm": {"on"}}, {"confirm": {"YES"}}} {
		_, _, body := u.post(u.c, "/system/update/install", form)
		if !strings.Contains(body, "Tick the box to confirm the update") {
			t.Fatalf("install %v not refused: %s", form, body)
		}
	}
	if _, installs, _ := u.up.counts(); installs != 0 {
		t.Fatalf("install called %d times without confirmation", installs)
	}
	if u.restarts() != 0 {
		t.Fatal("a refused install restarted the device")
	}
}

func TestUpdateInstallAndInstall(t *testing.T) {
	u := newUpdateHarness(t, UpdateStatus{Enabled: true, Current: "v0.1.0", Available: "v0.3.0"})
	_, path, body := u.post(u.c, "/system/update/install", url.Values{"confirm": {"yes"}})
	if path != "/system" || !strings.Contains(body, "Installing v0.3.0, the device restarts in a moment") {
		t.Fatalf("install: %s\n%s", path, body)
	}
	if _, installs, _ := u.up.counts(); installs != 1 {
		t.Fatalf("install calls = %d, want 1", installs)
	}
	waitFor(t, func() bool { return u.restarts() >= 1 })
	time.Sleep(50 * time.Millisecond) // the goroutine must not restart twice
	if u.restarts() != 1 {
		t.Fatalf("restarts = %d, want exactly 1", u.restarts())
	}

	// A failed install reports the error and does not restart the device.
	u.up.mu.Lock()
	u.up.instErr = errors.New("free space: 2 MB left, 24 MB needed")
	u.up.mu.Unlock()
	_, _, body = u.post(u.c, "/system/update/install", url.Values{"confirm": {"yes"}})
	if !strings.Contains(body, "Could not install the update") || !strings.Contains(body, "free space") {
		t.Fatalf("install error not reported: %s", body)
	}
	time.Sleep(700 * time.Millisecond)
	if u.restarts() != 1 {
		t.Fatalf("a failed install restarted the device (%d restarts)", u.restarts())
	}
}

func TestUpdateRollbackNeedsConfirmation(t *testing.T) {
	u := newUpdateHarness(t, UpdateStatus{Enabled: true, Current: "v0.2.0", CanRollback: true, RollbackTo: "v0.1.1"})
	_, _, body := u.post(u.c, "/system/update/rollback", url.Values{})
	if !strings.Contains(body, "Tick the box to confirm the rollback") {
		t.Fatalf("rollback not refused: %s", body)
	}
	if _, _, rolls := u.up.counts(); rolls != 0 {
		t.Fatalf("rollback called %d times without confirmation", rolls)
	}
	_, path, body := u.post(u.c, "/system/update/rollback", url.Values{"confirm": {"yes"}})
	if path != "/system" || !strings.Contains(body, "Going back to v0.1.1, the device restarts in a moment") {
		t.Fatalf("rollback: %s\n%s", path, body)
	}
	if _, _, rolls := u.up.counts(); rolls != 1 {
		t.Fatalf("rollback calls = %d, want 1", rolls)
	}
	waitFor(t, func() bool { return u.restarts() == 1 })
}

func TestUpdateRoutesNeedLogin(t *testing.T) {
	u := newUpdateHarness(t, UpdateStatus{Enabled: true, Current: "v0.1.0", Available: "v0.3.0"})
	anon := u.newClient()
	for _, p := range []string{"/system/update/check", "/system/update/install", "/system/update/rollback"} {
		req, _ := http.NewRequest(http.MethodPost, u.srv.URL+p, strings.NewReader("confirm=yes"))
		req.Header.Set("Content-Type", "application/x-www-form-urlencoded")
		req.Header.Set("Origin", u.srv.URL)
		resp, err := anon.Do(req)
		if err != nil {
			t.Fatal(err)
		}
		resp.Body.Close()
		if resp.StatusCode != http.StatusUnauthorized {
			t.Fatalf("%s without a session: %d, want 401", p, resp.StatusCode)
		}
	}
	// A browser form gets the login redirect instead (GET semantics of s.auth).
	if _, path, _ := u.get(u.newClient(), "/system"); path != "/login" {
		t.Fatalf("anonymous /system -> %s, want /login", path)
	}
	if checks, installs, rolls := u.up.counts(); checks+installs+rolls != 0 {
		t.Fatal("an anonymous request reached the updater")
	}
	if u.restarts() != 0 {
		t.Fatal("an anonymous request restarted the device")
	}
}

func TestUpdateRoutesRefuseForeignOrigin(t *testing.T) {
	u := newUpdateHarness(t, UpdateStatus{Enabled: true, Current: "v0.1.0", Available: "v0.3.0", CanRollback: true, RollbackTo: "v0.1.1"})
	for _, p := range []string{"/system/update/check", "/system/update/install", "/system/update/rollback"} {
		req, _ := http.NewRequest(http.MethodPost, u.srv.URL+p, strings.NewReader("confirm=yes"))
		req.Header.Set("Content-Type", "application/x-www-form-urlencoded")
		req.Header.Set("Origin", "http://evil.example")
		resp, err := u.c.Do(req)
		if err != nil {
			t.Fatal(err)
		}
		resp.Body.Close()
		if resp.StatusCode != http.StatusForbidden {
			t.Fatalf("%s from a foreign origin: %d, want 403", p, resp.StatusCode)
		}
	}
	if checks, installs, rolls := u.up.counts(); checks+installs+rolls != 0 {
		t.Fatal("a cross-site request reached the updater")
	}
	if u.restarts() != 0 {
		t.Fatal("a cross-site request restarted the device")
	}
}

func TestUpdateSettingsShownAndEnvReadOnly(t *testing.T) {
	u := newUpdateHarness(t, UpdateStatus{Enabled: true, Current: "v0.1.0"})
	_, _, body := u.get(u.c, "/system")
	if !strings.Contains(body, `action="/system/update/settings"`) {
		t.Fatalf("no settings form:\n%s", body)
	}
	if !strings.Contains(body, `<option value="off" selected>`) {
		t.Fatalf("the select does not show the default off:\n%s", body)
	}
	if strings.Contains(body, "read-only") {
		t.Fatalf("a clean config.json must not claim an override:\n%s", body)
	}
	u.env["WAT_UPDATE_CHECK"] = "daily"
	u.env["WAT_UPDATE_AUTO"] = "1"
	_, _, body = u.get(u.c, "/system")
	for _, want := range []string{`<option value="daily" selected>`, "set by WAT_UPDATE_CHECK", "set by WAT_UPDATE_AUTO"} {
		if !strings.Contains(body, want) {
			t.Fatalf("env override %q not shown:\n%s", want, body)
		}
	}
	if c := u.cfg.Get(); c.UpdateCheck != "" || c.UpdateAuto {
		t.Fatal("the UI persisted an environment override")
	}
}

func TestUpdateSettingsSaved(t *testing.T) {
	u := newUpdateHarness(t, UpdateStatus{Enabled: true, Current: "v0.1.0"})
	form := url.Values{"update_check": {"daily"}, "update_auto": {"1"}}
	if code, _, _ := u.post(u.c, "/system/update/settings", form); code != http.StatusOK {
		t.Fatalf("POST = %d", code)
	}
	if c := u.cfg.Get(); c.UpdateCheck != config.UpdateCheckDaily || !c.UpdateAuto {
		t.Fatalf("config = %+v, want daily + auto", c)
	}
	_, _, body := u.get(u.c, "/system")
	if !strings.Contains(body, `<option value="daily" selected>`) {
		t.Fatalf("the saved value is not shown:\n%s", body)
	}
	if !strings.Contains(body, `value="1" checked`) {
		t.Fatalf("the saved auto checkbox is not ticked:\n%s", body)
	}
	// An unknown mode is refused and nothing changes.
	if code, _, _ := u.post(u.c, "/system/update/settings", url.Values{"update_check": {"hourly"}}); code != http.StatusOK {
		t.Fatalf("POST bad mode = %d", code)
	}
	if c := u.cfg.Get(); c.UpdateCheck != config.UpdateCheckDaily || !c.UpdateAuto {
		t.Fatalf("a bad mode changed the config: %+v", c)
	}
}

func TestUpdateSettingsHiddenWithoutUpdater(t *testing.T) {
	h := newHarness(t)
	h.setup()
	if code, _, _ := h.post(h.c, "/system/update/settings", url.Values{"update_check": {"daily"}}); code != http.StatusNotFound {
		t.Fatalf("POST with no updater = %d, want 404", code)
	}
}

// The login page is the last resort when both the password and the phone are
// gone: it may erase the device only while nothing is paired.
func TestLoginPageResetOnlyWhileUnpaired(t *testing.T) {
	h := newHarness(t)
	h.setup() // a password exists, nothing paired
	anon := h.newClient()

	_, _, body := h.get(anon, "/login")
	if !strings.Contains(body, `action="/reset"`) || !strings.Contains(body, "Type RESET to confirm") {
		t.Fatalf("no reset block on the login page while unpaired:\n%s", body)
	}

	// The confirmation is required, and a wrong one changes nothing.
	if _, _, body := h.post(anon, "/reset", url.Values{"confirm": {"yes"}}); !strings.Contains(body, "Type RESET") {
		t.Fatalf("a missing confirmation was accepted:\n%s", body)
	}
	if n := h.resetCount(); n != 0 {
		t.Fatalf("resets = %d after a bad confirmation", n)
	}

	// The real thing: it erases and restarts, like !reset. The reset runs in
	// a goroutine after the response is flushed, so poll for it.
	_, _, body = h.post(anon, "/reset", url.Values{"confirm": {"RESET"}})
	if !strings.Contains(body, "Restarting") {
		t.Fatalf("no restart page:\n%s", body)
	}
	for i := 0; i < 40 && h.resetCount() == 0; i++ {
		time.Sleep(50 * time.Millisecond)
	}
	if n := h.resetCount(); n != 1 {
		t.Fatalf("resets = %d, want one", n)
	}
}

func TestLoginPageResetRefusedWhenPaired(t *testing.T) {
	h := newHarness(t)
	h.setup()
	h.wa.paired = true
	anon := h.newClient()

	_, _, body := h.get(anon, "/login")
	if strings.Contains(body, `action="/reset"`) {
		t.Fatalf("the login page offers a reset on a paired device:\n%s", body)
	}
	// And the route itself refuses even when the form is posted by hand.
	_, _, body = h.post(anon, "/reset", url.Values{"confirm": {"RESET"}})
	if n := h.resetCount(); n != 0 {
		t.Fatalf("resets = %d on a paired device", n)
	}
	if !strings.Contains(body, "linked to WhatsApp") {
		t.Fatalf("the refusal does not explain the way back:\n%s", body)
	}
}

// A device with no password is in setup mode: the login page is not the way
// in and the reset route must not be a second one.
func TestLoginResetWithoutPasswordGoesToSetup(t *testing.T) {
	h := newHarness(t)
	anon := h.newClient()
	_, path, _ := h.post(anon, "/reset", url.Values{"confirm": {"RESET"}})
	if path != "/setup" || h.resets != 0 {
		t.Fatalf("reset with no password -> %s, resets = %d", path, h.resets)
	}
}

// A foreign Origin is refused here too, like every other POST: an attacker's
// page must not be able to trigger the reset.
func TestLoginResetRefusesForeignOrigin(t *testing.T) {
	h := newHarness(t)
	h.setup()
	anon := h.newClient()
	req, _ := http.NewRequest(http.MethodPost, h.srv.URL+"/reset", strings.NewReader("confirm=RESET"))
	req.Header.Set("Content-Type", "application/x-www-form-urlencoded")
	req.Header.Set("Origin", "http://evil.example")
	resp, err := anon.Do(req)
	if err != nil {
		t.Fatal(err)
	}
	resp.Body.Close()
	if resp.StatusCode != http.StatusForbidden || h.resetCount() != 0 {
		t.Fatalf("status %d, resets %d", resp.StatusCode, h.resetCount())
	}
}
