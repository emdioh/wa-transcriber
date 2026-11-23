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

// fakeFlashInstaller is the internal storage service as the page sees it.
type fakeFlashInstaller struct {
	mu      sync.Mutex
	st      web_Flasher // alias so the field type is not the interface
	instErr error
	got     []string
	calls   int

	// For the "the page went away" test: Install announces itself on
	// started, waits for release, and records what its context looked like
	// when it was finally done.
	started   chan struct{}
	release   chan struct{}
	finished  chan struct{}
	cancelled chan struct{}
	ctx       context.Context
	ctxErr    error
}

// web_Flasher is what Status returns; a named alias keeps the struct field
// readable.
type web_Flasher = FlashStatus

func (f *fakeFlashInstaller) Status(context.Context) FlashStatus {
	f.mu.Lock()
	defer f.mu.Unlock()
	return f.st
}

func (f *fakeFlashInstaller) Install(ctx context.Context, confirm string) (FlashStatus, error) {
	f.mu.Lock()
	f.calls++
	f.got = append(f.got, confirm)
	st, err := f.st, f.instErr
	started, release, finished := f.started, f.release, f.finished
	f.mu.Unlock()
	if started != nil {
		f.mu.Lock()
		f.ctx, f.cancelled = ctx, make(chan struct{}, 1)
		cancelled := f.cancelled
		f.mu.Unlock()
		// Report a cancelled context the moment it happens, so the test can
		// tell "still running after the page left" from "killed with it".
		go func() {
			<-ctx.Done()
			cancelled <- struct{}{}
		}()
		started <- struct{}{}
		<-release
		f.mu.Lock()
		f.ctxErr = ctx.Err()
		f.mu.Unlock()
		if finished != nil {
			close(finished)
		}
	}
	return st, err
}

func (f *fakeFlashInstaller) confirmed() []string {
	f.mu.Lock()
	defer f.mu.Unlock()
	return append([]string(nil), f.got...)
}

// newFlashHarness is a server with an internal storage card.
func newFlashHarness(t *testing.T, st FlashStatus) (*harness, *fakeFlashInstaller) {
	t.Helper()
	cfg, err := config.Open(filepath.Join(t.TempDir(), "config.json"))
	if err != nil {
		t.Fatal(err)
	}
	h := &harness{t: t, cfg: cfg, st: status.New(time.Now()), wa: &fakeWA{},
		logs: logbuf.New(1<<16, nil), env: map[string]string{}}
	f := &fakeFlashInstaller{st: st}
	s := New(Options{
		Config: cfg, Status: h.st, Logs: h.logs, WA: h.wa,
		Getenv:         func(k string) string { return h.env[k] },
		Addrs:          func() []status.Addr { return nil },
		Version:        "test",
		FlashInstaller: f,
	})
	h.srv = httptest.NewServer(s.Handler())
	t.Cleanup(h.srv.Close)
	h.c = h.newClient()
	h.setup()
	return h, f
}

func TestFlashCardHiddenWithoutService(t *testing.T) {
	h := newHarness(t)
	h.setup()
	_, _, body := h.get(h.c, "/system")
	if strings.Contains(body, "Internal storage") {
		t.Fatal("the internal storage card is rendered without a service")
	}
	if code, _, _ := h.post(h.c, "/system/flash/install", url.Values{"confirm": {"mmcblk1"}}); code != http.StatusNotFound {
		t.Fatalf("install without a service: %d, want 404", code)
	}
}

func TestFlashCardHiddenWhenDisabled(t *testing.T) {
	h, _ := newFlashHarness(t, FlashStatus{Enabled: false})
	_, _, body := h.get(h.c, "/system")
	if strings.Contains(body, "Internal storage") {
		t.Fatal("a disabled installer still renders the card")
	}
}

func TestFlashCardShowsTheTarget(t *testing.T) {
	h, _ := newFlashHarness(t, FlashStatus{
		Enabled: true, Target: "/dev/mmcblk1", Size: 4 << 30, Need: 412 << 20,
		Install: "mmcblk1", Source: "/dev/mmcblk0", Marker: "wat-migrate",
	})
	_, _, body := h.get(h.c, "/system")
	for _, want := range []string{
		"Internal storage", "/dev/mmcblk1", "4 GB", "412 MB", "/dev/mmcblk0",
		"mmcblk1", "wat-migrate", "Install this card to the internal storage",
		`name="confirm"`, "remove the SD card",
	} {
		if !strings.Contains(body, want) {
			t.Fatalf("card missing %q:\n%s", want, body)
		}
	}
}

func TestFlashCardOneLineWithoutInternalFlash(t *testing.T) {
	// This is the Raspberry Pi 2: one MMC disk, the card it runs from.
	h, f := newFlashHarness(t, FlashStatus{
		Enabled: true,
		Error:   "This board has no internal storage: the only flash it has is the card it boots from.",
	})
	_, _, body := h.get(h.c, "/system")
	if !strings.Contains(body, "no internal storage") {
		t.Fatalf("the card does not say there is no internal storage:\n%s", body)
	}
	if strings.Contains(body, "Install this card to the internal storage") {
		t.Fatal("a board without internal flash is offered the button")
	}
	if code, _, _ := h.post(h.c, "/system/flash/install", url.Values{"confirm": {"mmcblk1"}}); code != http.StatusOK {
		t.Fatalf("install on a board without internal flash: %d", code)
	}
	if f.calls != 0 {
		t.Fatalf("the installer was called %d times without a target", f.calls)
	}
}

func TestFlashInstallNeedsTheTypedName(t *testing.T) {
	h, f := newFlashHarness(t, FlashStatus{
		Enabled: true, Target: "/dev/mmcblk1", Size: 4 << 30, Need: 412 << 20,
		Install: "mmcblk1", Source: "/dev/mmcblk0",
	})
	// No confirmation at all: the service is still asked, and it is the one
	// that refuses (it knows the device name), so the page never guesses.
	code, loc, page := h.post(h.c, "/system/flash/install", url.Values{})
	if code != http.StatusOK || loc != "/system" {
		t.Fatalf("install without confirm: %d %q", code, loc)
	}
	if !strings.Contains(page, "no internal storage") && !strings.Contains(page, "Failed") {
		// The fake returns nil error, so this is a success path; the real
		// refusal is covered in internal/flash. What matters here is that
		// the empty confirmation reached the service unchanged.
		_ = page
	}
	if got := f.confirmed(); len(got) != 1 || got[0] != "" {
		t.Fatalf("confirmations = %q, want one empty", got)
	}
	// Typing the name reaches the service as typed.
	if code, _, _ := h.post(h.c, "/system/flash/install", url.Values{"confirm": {"mmcblk1"}}); code != http.StatusOK {
		t.Fatalf("install: %d", code)
	}
	if got := f.confirmed(); len(got) != 2 || got[1] != "mmcblk1" {
		t.Fatalf("confirmations = %q", got)
	}
}

func TestFlashInstallReportsSuccess(t *testing.T) {
	h, _ := newFlashHarness(t, FlashStatus{
		Enabled: true, Target: "/dev/mmcblk1", Size: 4 << 30, Need: 412 << 20,
		Install: "mmcblk1", Source: "/dev/mmcblk0",
	})
	_, _, page := h.post(h.c, "/system/flash/install", url.Values{"confirm": {"mmcblk1"}})
	if !strings.Contains(page, "written and reads back identical") {
		t.Fatalf("success flash missing:\n%s", page)
	}
	if !strings.Contains(page, "remove the SD card") {
		t.Fatalf("the success flash does not say what to do next:\n%s", page)
	}
}

func TestFlashInstallFailureHasItsOwnWording(t *testing.T) {
	h, f := newFlashHarness(t, FlashStatus{
		Enabled: true, Target: "/dev/mmcblk1", Size: 4 << 30, Need: 412 << 20,
		Install: "mmcblk1", Source: "/dev/mmcblk0",
	})
	f.mu.Lock()
	f.instErr = errTest("flash: root-a (160 MB at 33792) was not written: input/output error")
	f.mu.Unlock()
	_, _, page := h.post(h.c, "/system/flash/install", url.Values{"confirm": {"mmcblk1"}})
	if !strings.Contains(page, "not bootable yet") || !strings.Contains(page, "still boots") {
		t.Fatalf("a failed install does not say the card still boots:\n%s", page)
	}
}

func TestFlashCardBusyHidesTheButton(t *testing.T) {
	h, _ := newFlashHarness(t, FlashStatus{
		Enabled: true, Target: "/dev/mmcblk1", Size: 4 << 30, Need: 412 << 20,
		Install: "mmcblk1", Source: "/dev/mmcblk0", Busy: true,
	})
	_, _, body := h.get(h.c, "/system")
	if strings.Contains(body, "Install this card to the internal storage") {
		t.Fatal("the button is offered while an install runs")
	}
	if !strings.Contains(body, "In progress") {
		t.Fatalf("the card does not say an install runs:\n%s", body)
	}
}

// errTest is a plain error for the fake.
type errTest string

func (e errTest) Error() string { return string(e) }

// The install is a disk write that takes minutes: the phone's screen going off
// or a reload cancels the HTTP request, and that must not cancel the copy. The
// handler detaches the context from the request, so the installer's context is
// still alive after the client is gone.
func TestFlashInstallKeepsRunningWhenThePageGoesAway(t *testing.T) {
	h, f := newFlashHarness(t, FlashStatus{
		Enabled: true, Target: "/dev/mmcblk1", Size: 4 << 30, Need: 412 << 20,
		Install: "mmcblk1", Source: "/dev/mmcblk0",
	})
	f.mu.Lock()
	f.started = make(chan struct{}, 1)
	f.release = make(chan struct{})
	f.finished = make(chan struct{})
	f.mu.Unlock()

	form := url.Values{"confirm": {"mmcblk1"}}.Encode()
	reqCtx, cancelReq := context.WithCancel(t.Context())
	req, err := http.NewRequestWithContext(reqCtx, http.MethodPost,
		h.srv.URL+"/system/flash/install", strings.NewReader(form))
	if err != nil {
		t.Fatal(err)
	}
	req.Header.Set("Content-Type", "application/x-www-form-urlencoded")
	req.Header.Set("Origin", h.srv.URL)
	served := make(chan struct{})
	go func() {
		defer close(served)
		resp, err := h.c.Do(req)
		if err == nil {
			resp.Body.Close()
		}
	}()

	<-f.started // the install has begun
	cancelReq() // the screen went off / the tab reloaded
	// A failing assertion must still let the handler finish: the test server's
	// Close waits for it, so release exactly once and wait for the installer.
	var once sync.Once
	release := func() { once.Do(func() { close(f.release) }) }
	defer func() {
		release()
		<-f.finished
	}()
	// Give the server a moment to notice the client is gone. With the request
	// context wired straight in, the installer's context dies right here;
	// with the context detached it never does.
	select {
	case <-f.cancelled:
		release()
		<-f.finished
		t.Fatal("the install's context was cancelled with the page")
	case <-time.After(500 * time.Millisecond):
	}
	release()
	<-f.finished
	<-served

	if got := f.confirmed(); len(got) != 1 {
		t.Fatalf("installer calls = %d, want 1", len(got))
	}
	if f.ctxErr != nil {
		t.Fatalf("the install was cancelled with the page: %v", f.ctxErr)
	}
}

// A page loaded while an install runs must show that it runs and how far it
// got, not the form again.
func TestFlashCardShowsProgress(t *testing.T) {
	for _, c := range []struct {
		name        string
		done, total int64
		want        []string
	}{
		{"copying", 96 << 20, 204 << 20, []string{"In progress", "copying", "47 %", "96 MB of 102 MB"}},
		{"checking", 150 << 20, 204 << 20, []string{"In progress", "checking the copy", "73 %", "102 MB of 102 MB"}},
	} {
		t.Run(c.name, func(t *testing.T) {
			h, _ := newFlashHarness(t, FlashStatus{
				Enabled: true, Target: "/dev/mmcblk1", Size: 4 << 30, Need: 102 << 20,
				Install: "mmcblk1", Source: "/dev/mmcblk0", Busy: true,
				ProgressDone: c.done, ProgressTotal: c.total,
			})
			_, _, body := h.get(h.c, "/system")
			for _, want := range c.want {
				if !strings.Contains(body, want) {
					t.Errorf("page missing %q:\n%s", want, body)
				}
			}
			// The page refreshes itself only while an install runs.
			if !strings.Contains(body, `data-flash="busy"`) {
				t.Error("the card does not ask the page to follow the install")
			}
		})
	}
}

// After the install the page has to say so, because the page that started it
// may be long gone.
func TestFlashCardShowsTheOutcome(t *testing.T) {
	done := time.Date(2026, 11, 23, 14, 5, 0, 0, time.UTC)
	h, _ := newFlashHarness(t, FlashStatus{
		Enabled: true, Target: "/dev/mmcblk1", Size: 4 << 30, Need: 412 << 20,
		Install: "mmcblk1", Source: "/dev/mmcblk0", Done: true, DoneAt: done,
	})
	_, _, body := h.get(h.c, "/system")
	for _, want := range []string{"✓", "written and reads back identical", "remove the SD card"} {
		if !strings.Contains(body, want) {
			t.Errorf("page missing %q:\n%s", want, body)
		}
	}
	if strings.Contains(body, `data-flash="busy"`) {
		t.Error("a finished install still asks the page to refresh")
	}
}
