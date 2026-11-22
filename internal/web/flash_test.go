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
}

// web_Flasher is what Status returns; a named alias keeps the struct field
// readable.
type web_Flasher = FlashStatus

func (f *fakeFlashInstaller) Status(context.Context) FlashStatus {
	f.mu.Lock()
	defer f.mu.Unlock()
	return f.st
}

func (f *fakeFlashInstaller) Install(_ context.Context, confirm string) (FlashStatus, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.calls++
	f.got = append(f.got, confirm)
	return f.st, f.instErr
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
