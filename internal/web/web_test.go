package web

import (
	"context"
	"io"
	"net/http"
	"net/http/cookiejar"
	"net/http/httptest"
	"net/url"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"

	"go.mau.fi/whatsmeow/types"
	"golang.org/x/crypto/bcrypt"

	"github.com/emdioh/wa-transcriber/internal/config"
	"github.com/emdioh/wa-transcriber/internal/logbuf"
	"github.com/emdioh/wa-transcriber/internal/status"
)

func init() { loginFailDelay = 0 }

type fakeWA struct {
	mu      sync.Mutex
	paired  bool
	phone   string
	qr      bool
	lidToPN map[types.JID]types.JID
	names   map[types.JID]string
	pairErr error
}

func (f *fakeWA) Paired() bool { return f.paired }
func (f *fakeWA) RequestPairingCode(_ context.Context, phone string) error {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.phone = phone
	return f.pairErr
}
func (f *fakeWA) UseQR()                                         { f.qr = true }
func (f *fakeWA) ChatName(_ context.Context, j types.JID) string { return f.names[j] }
func (f *fakeWA) CanonicalChat(_ context.Context, j types.JID) types.JID {
	if pn, ok := f.lidToPN[j]; ok {
		return pn
	}
	return j
}

type harness struct {
	t      *testing.T
	srv    *httptest.Server
	c      *http.Client
	cfg    *config.Store
	st     *status.Tracker
	wa     *fakeWA
	logs   *logbuf.Buffer
	env    map[string]string
	resets int
	unlink int
	mu     sync.Mutex
}

func newHarness(t *testing.T) *harness {
	t.Helper()
	cfg, err := config.Open(filepath.Join(t.TempDir(), "config.json"))
	if err != nil {
		t.Fatal(err)
	}
	h := &harness{t: t, cfg: cfg, st: status.New(time.Now()), wa: &fakeWA{}, logs: logbuf.New(1<<16, nil), env: map[string]string{}}
	s := New(Options{
		Config: cfg, Status: h.st, Logs: h.logs, WA: h.wa,
		Getenv:  func(k string) string { return h.env[k] },
		Reset:   func(context.Context) error { h.mu.Lock(); h.resets++; h.mu.Unlock(); return nil },
		Unlink:  func(context.Context) error { h.mu.Lock(); h.unlink++; h.mu.Unlock(); return nil },
		Addrs:   func() []status.Addr { return []status.Addr{{Interface: "eth0", IP: "10.0.0.2"}} },
		Version: "test",
	})
	h.srv = httptest.NewServer(s.Handler())
	t.Cleanup(h.srv.Close)
	h.c = h.newClient()
	return h
}

func (h *harness) newClient() *http.Client {
	jar, _ := cookiejar.New(nil)
	return &http.Client{Jar: jar}
}

func (h *harness) get(c *http.Client, path string) (int, string, string) {
	h.t.Helper()
	resp, err := c.Get(h.srv.URL + path)
	if err != nil {
		h.t.Fatal(err)
	}
	defer resp.Body.Close()
	body, _ := io.ReadAll(resp.Body)
	return resp.StatusCode, resp.Request.URL.Path, string(body)
}

func (h *harness) post(c *http.Client, path string, form url.Values) (int, string, string) {
	h.t.Helper()
	req, _ := http.NewRequest(http.MethodPost, h.srv.URL+path, strings.NewReader(form.Encode()))
	req.Header.Set("Content-Type", "application/x-www-form-urlencoded")
	req.Header.Set("Origin", h.srv.URL)
	resp, err := c.Do(req)
	if err != nil {
		h.t.Fatal(err)
	}
	defer resp.Body.Close()
	body, _ := io.ReadAll(resp.Body)
	return resp.StatusCode, resp.Request.URL.Path, string(body)
}

func (h *harness) setup() {
	h.t.Helper()
	_, path, _ := h.post(h.c, "/setup", url.Values{"password": {"correct horse"}, "confirm": {"correct horse"}})
	if path != "/whatsapp" {
		h.t.Fatalf("after setup: %s", path)
	}
}

func TestEverythingLeadsToSetupFirst(t *testing.T) {
	h := newHarness(t)
	for _, p := range []string{"/", "/transcription", "/login"} {
		if _, path, _ := h.get(h.c, p); path != "/setup" {
			t.Errorf("%s -> %s, want /setup", p, path)
		}
	}
	_, _, body := h.post(h.c, "/setup", url.Values{"password": {"short"}, "confirm": {"short"}})
	if !strings.Contains(body, "at least 8") {
		t.Fatal("short password accepted")
	}
	h.setup()
	if _, path, _ := h.get(h.c, "/setup"); path != "/login" && path != "/" {
		// logged in: /setup -> /login -> (session valid) stays on login page; both are fine
		t.Fatalf("/setup after setup -> %s", path)
	}
	other := h.newClient()
	h.post(other, "/setup", url.Values{"password": {"attacker!!"}, "confirm": {"attacker!!"}})
	hash := h.cfg.Get().Web.PasswordHash
	if bcrypt.CompareHashAndPassword([]byte(hash), []byte("correct horse")) != nil {
		t.Fatal("a second setup replaced the password")
	}
}

func TestLoginRequiredAndWorks(t *testing.T) {
	h := newHarness(t)
	h.setup()
	anon := h.newClient()
	if _, path, _ := h.get(anon, "/transcription"); path != "/login" {
		t.Fatalf("anonymous -> %s", path)
	}
	if code, _, _ := h.get(anon, "/api/status"); code != http.StatusUnauthorized {
		t.Fatalf("api without session: %d", code)
	}
	_, _, body := h.post(anon, "/login", url.Values{"password": {"nope"}, "next": {"/transcription"}})
	if !strings.Contains(body, "Wrong password") {
		t.Fatal("wrong password not reported")
	}
	_, path, _ := h.post(anon, "/login", url.Values{"password": {"correct horse"}, "next": {"/transcription"}})
	if path != "/transcription" {
		t.Fatalf("after login -> %s", path)
	}
	_, path, _ = h.post(anon, "/login", url.Values{"password": {"correct horse"}, "next": {"//evil.example"}})
	if path != "/" {
		t.Fatalf("open redirect: %s", path)
	}
}

func TestPasswordChangeLogsOutOtherSessions(t *testing.T) {
	h := newHarness(t)
	h.setup()
	second := h.newClient()
	h.post(second, "/login", url.Values{"password": {"correct horse"}})
	if _, path, _ := h.get(second, "/system"); path != "/system" {
		t.Fatal("second browser not logged in")
	}
	_, _, body := h.post(h.c, "/system/password", url.Values{"current": {"wrong"}, "password": {"newpassword"}, "confirm": {"newpassword"}})
	if !strings.Contains(body, "current password is wrong") {
		t.Fatal("wrong current password accepted")
	}
	_, _, body = h.post(h.c, "/system/password", url.Values{"current": {"correct horse"}, "password": {"newpassword"}, "confirm": {"newpassword"}})
	if !strings.Contains(body, "Password changed") {
		t.Fatalf("change failed: %s", body)
	}
	if _, path, _ := h.get(h.c, "/system"); path != "/system" {
		t.Fatal("the browser that changed the password must stay logged in")
	}
	if _, path, _ := h.get(second, "/system"); path != "/login" {
		t.Fatal("other session survived the password change")
	}
	// A change from WhatsApp (!password) has the same effect.
	_ = h.cfg.Update(func(c *config.Config) { c.Web.PasswordHash = "$2a$10$something-else" })
	if _, path, _ := h.get(h.c, "/system"); path != "/login" {
		t.Fatal("session survived an out-of-band password change")
	}
}

func TestCrossSitePostRefused(t *testing.T) {
	h := newHarness(t)
	h.setup()
	req, _ := http.NewRequest(http.MethodPost, h.srv.URL+"/system/reset", strings.NewReader("confirm=RESET"))
	req.Header.Set("Content-Type", "application/x-www-form-urlencoded")
	req.Header.Set("Origin", "http://evil.example")
	resp, err := h.c.Do(req)
	if err != nil {
		t.Fatal(err)
	}
	resp.Body.Close()
	if resp.StatusCode != http.StatusForbidden || h.resets != 0 {
		t.Fatalf("status %d, resets %d", resp.StatusCode, h.resets)
	}
}

func TestTranscriptionForm(t *testing.T) {
	h := newHarness(t)
	h.setup()
	_ = h.cfg.Update(func(c *config.Config) { c.Transcription.APIKey = "sk-old" })
	form := url.Values{
		"provider": {"gemini"}, "api_key": {""}, "keep_languages": {"Italian, en, it"},
		"translate_to": {"IT"}, "global": {"on"}, "timeout_seconds": {"90"},
	}
	_, _, body := h.post(h.c, "/transcription", form)
	if !strings.Contains(body, "Settings saved") {
		t.Fatalf("save failed: %s", body)
	}
	tr := h.cfg.Get().Transcription
	if tr.Provider != "gemini" || tr.APIKey != "sk-old" || strings.Join(tr.KeepLanguages, ",") != "it,en" ||
		tr.TranslateTo != "it" || !tr.Global || tr.TimeoutSeconds != 90 {
		t.Fatalf("saved = %+v", tr)
	}
	if strings.Contains(body, "sk-old") {
		t.Fatal("the API key must never be sent back to the browser")
	}

	form.Set("keep_languages", "klingon")
	_, _, body = h.post(h.c, "/transcription", form)
	if !strings.Contains(body, "Unknown language(s): klingon") {
		t.Fatal("bad language accepted")
	}
	form.Set("keep_languages", "it")
	form.Set("clear_key", "on")
	h.post(h.c, "/transcription", form)
	if h.cfg.Get().Transcription.APIKey != "" {
		t.Fatal("key not cleared")
	}
}

func TestEnvOverriddenFieldsAreReadOnly(t *testing.T) {
	h := newHarness(t)
	h.setup()
	h.env["WAT_API_KEY"] = "sk-env"
	_, _, body := h.get(h.c, "/transcription")
	if !strings.Contains(body, "set by WAT_API_KEY") || strings.Contains(body, "sk-env") {
		t.Fatal("env override not shown read-only, or key leaked")
	}
}

func TestChatOverridesGroupedAndEditable(t *testing.T) {
	h := newHarness(t)
	h.setup()
	pn := types.NewJID("393331111111", types.DefaultUserServer)
	lid := types.NewJID("777", types.HiddenUserServer)
	h.wa.lidToPN = map[types.JID]types.JID{lid: pn}
	h.wa.names = map[types.JID]string{pn: "Anna"}
	_ = h.cfg.Update(func(c *config.Config) {
		c.Transcription.Chats = map[string]bool{pn.String(): true, lid.String(): true}
	})
	_, _, body := h.get(h.c, "/transcription")
	if strings.Count(body, "Turn off") != 1 || !strings.Contains(body, "Anna") || !strings.Contains(body, "&#43;393331111111") {
		t.Fatalf("expected one row for Anna:\n%s", body)
	}
	h.post(h.c, "/transcription/chat", url.Values{"key": {pn.String(), lid.String()}, "action": {"off"}})
	chats := h.cfg.Get().Transcription.Chats
	if chats[pn.String()] || chats[lid.String()] || len(chats) != 2 {
		t.Fatalf("chats = %v", chats)
	}
	h.post(h.c, "/transcription/chat", url.Values{"key": {pn.String(), lid.String(), "injected@s.whatsapp.net"}, "action": {"remove"}})
	if n := len(h.cfg.Get().Transcription.Chats); n != 0 {
		t.Fatalf("chats left: %d", n)
	}
}

func TestWhatsAppPairing(t *testing.T) {
	h := newHarness(t)
	h.setup()
	h.st.SetPairingCode("qr:2@abcdef,ghijk,lmnop")
	_, _, body := h.get(h.c, "/whatsapp")
	if !strings.Contains(body, "Scan with WhatsApp") {
		t.Fatal("QR not offered")
	}
	resp, err := h.c.Get(h.srv.URL + "/whatsapp/qr.svg")
	if err != nil {
		t.Fatal(err)
	}
	svg, _ := io.ReadAll(resp.Body)
	resp.Body.Close()
	if resp.Header.Get("Content-Type") != "image/svg+xml" || !strings.HasPrefix(string(svg), "<svg") {
		t.Fatalf("qr.svg: %s %q", resp.Header.Get("Content-Type"), svg[:20])
	}

	_, _, body = h.post(h.c, "/whatsapp/phone", url.Values{"phone": {"0333 123"}})
	if !strings.Contains(body, "country code") || h.wa.phone != "" {
		t.Fatal("number without country code accepted")
	}
	h.post(h.c, "/whatsapp/phone", url.Values{"phone": {"+39 333 1234567"}})
	if h.wa.phone != "+39 333 1234567" {
		t.Fatalf("phone = %q", h.wa.phone)
	}
	h.st.SetPairingCode("code:ABCD1234")
	_, _, body = h.get(h.c, "/whatsapp")
	if !strings.Contains(body, "ABCD-1234") {
		t.Fatal("pairing code not shown")
	}

	h.wa.paired = true
	h.st.SetPairingCode("")
	if code, _, _ := h.get(h.c, "/whatsapp/qr.svg"); code != http.StatusNotFound {
		t.Fatal("QR served while paired")
	}
	_, _, body = h.post(h.c, "/whatsapp/unlink", url.Values{})
	if !strings.Contains(body, "Tick the box") {
		t.Fatal("unlink without confirmation")
	}
	h.post(h.c, "/whatsapp/unlink", url.Values{"confirm": {"yes"}})
	waitFor(t, func() bool { h.mu.Lock(); defer h.mu.Unlock(); return h.unlink == 1 })
}

func TestResetNeedsTypedConfirmation(t *testing.T) {
	h := newHarness(t)
	h.setup()
	h.post(h.c, "/system/reset", url.Values{"confirm": {"reset"}})
	h.post(h.c, "/system/reset", url.Values{"confirm": {"RESET"}})
	waitFor(t, func() bool { h.mu.Lock(); defer h.mu.Unlock(); return h.resets == 1 })
}

func TestLogsFilterAndDownload(t *testing.T) {
	h := newHarness(t)
	h.setup()
	h.logs.Write([]byte("time=1 level=INFO msg=hello\ntime=2 level=ERROR msg=boom\n"))
	_, _, body := h.get(h.c, "/logs?level=error")
	if strings.Contains(body, "msg=hello") || !strings.Contains(body, "msg=boom") {
		t.Fatal("filter not applied")
	}
	resp, _ := h.c.Get(h.srv.URL + "/logs.txt")
	data, _ := io.ReadAll(resp.Body)
	resp.Body.Close()
	if !strings.Contains(string(data), "msg=hello") || !strings.Contains(resp.Header.Get("Content-Disposition"), "attachment") {
		t.Fatal("download incomplete")
	}
}

func TestSecurityHeaders(t *testing.T) {
	h := newHarness(t)
	resp, err := h.c.Get(h.srv.URL + "/setup")
	if err != nil {
		t.Fatal(err)
	}
	resp.Body.Close()
	if !strings.Contains(resp.Header.Get("Content-Security-Policy"), "script-src 'self'") ||
		resp.Header.Get("X-Frame-Options") != "DENY" {
		t.Fatal("security headers missing")
	}
}

func waitFor(t *testing.T, cond func() bool) {
	t.Helper()
	deadline := time.Now().Add(3 * time.Second)
	for !cond() {
		if time.Now().After(deadline) {
			t.Fatal("condition not met")
		}
		time.Sleep(20 * time.Millisecond)
	}
}
