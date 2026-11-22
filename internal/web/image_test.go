package web

import (
	"context"
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

// fakeImageUpdater is the image service as the page sees it.
type fakeImageUpdater struct {
	mu       sync.Mutex
	st       ImageStatus
	checkErr error
	instErr  error
	rollErr  error
	checks   int
	installs int
	rolls    int
}

func (f *fakeImageUpdater) get() ImageStatus {
	f.mu.Lock()
	defer f.mu.Unlock()
	return f.st
}

func (f *fakeImageUpdater) Status(context.Context) ImageStatus { return f.get() }

func (f *fakeImageUpdater) Check(context.Context) (ImageStatus, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.checks++
	return f.st, f.checkErr
}

func (f *fakeImageUpdater) Install(context.Context) (ImageStatus, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.installs++
	return f.st, f.instErr
}

func (f *fakeImageUpdater) Rollback(context.Context) (ImageStatus, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.rolls++
	return f.st, f.rollErr
}

func (f *fakeImageUpdater) counts() (checks, installs, rolls int) {
	f.mu.Lock()
	defer f.mu.Unlock()
	return f.checks, f.installs, f.rolls
}

// imageHarness is newHarness plus an image updater and a Restart counter.
type imageHarness struct {
	*harness
	up      *fakeImageUpdater
	rMu     sync.Mutex
	restart int
}

func newImageHarness(t *testing.T, st ImageStatus) *imageHarness {
	t.Helper()
	cfg, err := config.Open(filepath.Join(t.TempDir(), "config.json"))
	if err != nil {
		t.Fatal(err)
	}
	h := &harness{t: t, cfg: cfg, st: status.New(time.Now()), wa: &fakeWA{},
		logs: logbuf.New(1<<16, nil), env: map[string]string{}}
	u := &imageHarness{harness: h, up: &fakeImageUpdater{st: st}}
	s := New(Options{
		Config: cfg, Status: h.st, Logs: h.logs, WA: h.wa,
		Getenv:       func(k string) string { return h.env[k] },
		Addrs:        func() []status.Addr { return nil },
		Version:      "test",
		ImageUpdater: u.up,
		Restart:      func() { u.rMu.Lock(); u.restart++; u.rMu.Unlock() },
	})
	h.srv = httptest.NewServer(s.Handler())
	t.Cleanup(h.srv.Close)
	h.c = h.newClient()
	h.setup()
	return u
}

func (u *imageHarness) restarts() int {
	u.rMu.Lock()
	defer u.rMu.Unlock()
	return u.restart
}

func TestImageCardHiddenWithoutService(t *testing.T) {
	h := newHarness(t)
	h.setup()
	_, _, body := h.get(h.c, "/system")
	if strings.Contains(body, "System image") {
		t.Fatal("the image card is rendered without a service")
	}
	for _, p := range []string{"/system/image/check", "/system/image/install", "/system/image/rollback"} {
		code, _, _ := h.post(h.c, p, url.Values{"confirm": {"yes"}})
		if code != http.StatusNotFound {
			t.Fatalf("%s without a service: %d, want 404", p, code)
		}
	}
}

func TestImageCardHiddenWhenDisabled(t *testing.T) {
	u := newImageHarness(t, ImageStatus{Enabled: false})
	_, _, body := u.get(u.c, "/system")
	if strings.Contains(body, "System image") {
		t.Fatal("a disabled image service still renders the card")
	}
	if code, _, _ := u.post(u.c, "/system/image/check", url.Values{}); code != http.StatusNotFound {
		t.Fatalf("check while disabled: %d", code)
	}
}

func TestImageCardShowsSlotsAndImage(t *testing.T) {
	u := newImageHarness(t, ImageStatus{
		Enabled: true, Current: "v1.1.0", Ready: true,
		Slot: "A", OtherSlot: "B", Bootable: false,
		Available: "v1.2.0", ImageSize: 184 << 20, Installable: true,
		NotesURL: "https://example.invalid/notes",
	})
	_, _, body := u.get(u.c, "/system")
	for _, want := range []string{
		"System image", "slot A", "v1.1.0", "slot B", "no image installed",
		"v1.2.0", "184 MB", "Install and restart", "Check for a new image",
		"https://example.invalid/notes",
	} {
		if !strings.Contains(body, want) {
			t.Fatalf("card missing %q:\n%s", want, body)
		}
	}
	if strings.Contains(body, "Back to the previous image") {
		t.Fatal("a rollback is offered without a previous image")
	}
}

func TestImageCardReflashLineOnOldLayout(t *testing.T) {
	u := newImageHarness(t, ImageStatus{
		Enabled: true, Current: "v1.0.0",
		LayoutError: "reflash once to enable image updates",
	})
	_, _, body := u.get(u.c, "/system")
	if !strings.Contains(body, "reflash once to enable image updates") {
		t.Fatalf("the card does not say to reflash:\n%s", body)
	}
	for _, not := range []string{"Install and restart", "Back to the previous image"} {
		if strings.Contains(body, not) {
			t.Fatalf("an old layout still shows %q", not)
		}
	}
	// The check button is offered but explains, and never guesses: the
	// service is called and its answer is what the flash says.
	code, _, page := u.post(u.c, "/system/image/check", url.Values{})
	if code != http.StatusOK {
		t.Fatalf("check on an old layout: %d", code)
	}
	if !strings.Contains(page, "reflash once to enable image updates") {
		t.Fatalf("the check did not explain the old layout:\n%s", page)
	}
	if checks, _, _ := u.up.counts(); checks != 1 {
		t.Fatalf("checks = %d, want 1", checks)
	}
}

func TestImageInstallRestartsAfterTheResponse(t *testing.T) {
	u := newImageHarness(t, ImageStatus{
		Enabled: true, Ready: true, Slot: "A", OtherSlot: "B",
		Available: "v1.2.0", Installable: true,
	})
	// post follows the redirect: 200 is the /system page it lands on.
	if code, _, _ := u.post(u.c, "/system/image/install", url.Values{}); code != http.StatusOK {
		t.Fatalf("install without confirmation: %d", code)
	}
	if _, installs, _ := u.up.counts(); installs != 0 {
		t.Fatal("install ran without the confirmation box")
	}
	// The flash cookie is one-shot: the page post() lands on is the only
	// read that carries it.
	code, loc, page := u.post(u.c, "/system/image/install", url.Values{"confirm": {"yes"}})
	if code != http.StatusOK || loc != "/system" {
		t.Fatalf("install: %d %q", code, loc)
	}
	if !strings.Contains(page, "installed on the other slot") {
		t.Fatalf("install flash missing:\n%s", page)
	}
	if _, installs, _ := u.up.counts(); installs != 1 {
		t.Fatalf("installs = %d, want 1", installs)
	}
	// The restart happens in the background, after the response.
	deadline := time.Now().Add(2 * time.Second)
	for u.restarts() == 0 && time.Now().Before(deadline) {
		time.Sleep(10 * time.Millisecond)
	}
	if u.restarts() != 1 {
		t.Fatalf("restarts = %d, want 1", u.restarts())
	}
}

func TestImageInstallFailureKeepsTheDevice(t *testing.T) {
	u := newImageHarness(t, ImageStatus{
		Enabled: true, Ready: true, Slot: "A", OtherSlot: "B",
		Available: "v1.2.0", Installable: true,
	})
	u.up.instErr = context.DeadlineExceeded
	code, _, page := u.post(u.c, "/system/image/install", url.Values{"confirm": {"yes"}})
	if code != http.StatusOK {
		t.Fatalf("failed install: %d", code)
	}
	time.Sleep(2 * updateRestartDelay)
	if u.restarts() != 0 {
		t.Fatal("a failed install restarted the device")
	}
	if !strings.Contains(page, "Could not install the image") {
		t.Fatalf("the failure is not reported:\n%s", page)
	}
}

func TestImageRollback(t *testing.T) {
	u := newImageHarness(t, ImageStatus{
		Enabled: true, Ready: true, Slot: "B", OtherSlot: "A",
		CanRollback: true, RollbackTo: "A",
	})
	_, _, body := u.get(u.c, "/system")
	if !strings.Contains(body, "Back to the previous image") || !strings.Contains(body, "slot A") {
		t.Fatalf("the rollback button is missing:\n%s", body)
	}
	if code, _, _ := u.post(u.c, "/system/image/rollback", url.Values{"confirm": {"yes"}}); code != http.StatusOK {
		t.Fatalf("rollback: %d", code)
	}
	if _, _, rolls := u.up.counts(); rolls != 1 {
		t.Fatalf("rollbacks = %d, want 1", rolls)
	}
	deadline := time.Now().Add(2 * time.Second)
	for u.restarts() == 0 && time.Now().Before(deadline) {
		time.Sleep(10 * time.Millisecond)
	}
	if u.restarts() != 1 {
		t.Fatal("a rollback did not restart the device")
	}
}

func TestImageCheckReportsNewImage(t *testing.T) {
	u := newImageHarness(t, ImageStatus{Enabled: true, Ready: true, Available: "v1.2.0", Installable: true})
	code, _, page := u.post(u.c, "/system/image/check", url.Values{})
	if code != http.StatusOK {
		t.Fatalf("check: %d", code)
	}
	if !strings.Contains(page, "Image v1.2.0 is available") {
		t.Fatalf("the check flash is missing:\n%s", page)
	}
	// Nothing to install: the check says so.
	u.up.st.Installable = false
	u.up.st.Available = ""
	_, _, page = u.post(u.c, "/system/image/check", url.Values{})
	if !strings.Contains(page, "No new image for this device") {
		t.Fatalf("the empty check flash is missing:\n%s", page)
	}
}

func TestSizeText(t *testing.T) {
	for _, tc := range []struct {
		in   int64
		want string
	}{
		{0, ""}, {-1, ""}, {1024, "1 kB"}, {184 << 20, "184 MB"},
	} {
		if got := sizeText(tc.in); got != tc.want {
			t.Fatalf("sizeText(%d) = %q, want %q", tc.in, got, tc.want)
		}
	}
}
