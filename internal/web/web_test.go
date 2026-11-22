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

// resetCount reads the reset counter under the mutex: the reset itself runs in
// a goroutine after the response, so a test that polls for it races otherwise.
func (h *harness) resetCount() int {
	h.mu.Lock()
	defer h.mu.Unlock()
	return h.resets
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
	// The saved key is shown in plain text, so the owner can see what is stored.
	_, _, body := h.get(h.c, "/transcription")
	if !strings.Contains(body, `type="text" name="api_key"`) || !strings.Contains(body, `value="sk-old"`) {
		t.Fatal("the saved key is not prefilled in the form")
	}
	if !strings.Contains(body, "Delete the saved key") {
		t.Fatal("no way to delete the saved key")
	}
	// An empty field still keeps a key saved out of band (old browsers, scripts).
	_ = h.cfg.Update(func(c *config.Config) { c.Transcription.APIKey = "sk-newer" })
	_, _, body = h.post(h.c, "/transcription", form)
	if !strings.Contains(body, "Settings saved") {
		t.Fatalf("save failed: %s", body)
	}
	tr := h.cfg.Get().Transcription
	if tr.Provider != "gemini" || tr.APIKey != "sk-newer" || strings.Join(tr.KeepLanguages, ",") != "it,en" ||
		tr.TranslateTo != "it" || !tr.Global || tr.TimeoutSeconds != 90 {
		t.Fatalf("saved = %+v", tr)
	}

	// The prefilled value comes back and re-saves the same key.
	_, _, body = h.post(h.c, "/transcription", url.Values{
		"provider": {"gemini"}, "api_key": {"sk-newer"}, "keep_languages": {"it"},
	})
	if !strings.Contains(body, "Settings saved") || h.cfg.Get().Transcription.APIKey != "sk-newer" {
		t.Fatal("re-saving the prefilled key failed")
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
	if _, _, body := h.get(h.c, "/transcription"); strings.Contains(body, `value="sk-newer"`) {
		t.Fatal("the cleared key is still prefilled")
	}
}

func TestEnvOverriddenFieldsAreReadOnly(t *testing.T) {
	h := newHarness(t)
	h.setup()
	_ = h.cfg.Update(func(c *config.Config) { c.Transcription.APIKey = "sk-saved" })
	h.env["WAT_API_KEY"] = "sk-env"
	_, _, body := h.get(h.c, "/transcription")
	if !strings.Contains(body, "set by WAT_API_KEY") || strings.Contains(body, "sk-env") {
		t.Fatal("env override not shown read-only, or key leaked")
	}
	// A key from the environment must never be printed, not even a saved one.
	if strings.Contains(body, "sk-saved") || strings.Contains(body, `value="sk-`) {
		t.Fatal("a key value leaked into the page while WAT_API_KEY is set")
	}
	if strings.Contains(body, `type="text" name="api_key"`) {
		t.Fatal("the env-provided key must stay hidden")
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
	if !strings.Contains(body, `data-level="error"`) || !strings.Contains(body, `id="logstamp"`) {
		t.Fatal("the live view has no level or no stamp to update")
	}
	resp, _ := h.c.Get(h.srv.URL + "/logs.txt")
	data, _ := io.ReadAll(resp.Body)
	resp.Body.Close()
	if !strings.Contains(string(data), "msg=hello") || !strings.Contains(resp.Header.Get("Content-Disposition"), "attachment") {
		t.Fatal("download incomplete")
	}
}

func TestLogsLive(t *testing.T) {
	h := newHarness(t)
	h.logs.Write([]byte("time=1 level=INFO msg=hello\ntime=2 level=WARN msg=careful\ntime=3 level=ERROR msg=boom\n"))

	// Auth: the live view never leaks logs to an anonymous client.
	if code, _, _ := h.get(h.newClient(), "/logs/live"); code != http.StatusOK {
		// it redirects to /login, which the helper follows; the body must not
		// carry logs either way
		t.Fatalf("anonymous /logs/live: %d", code)
	}
	if _, _, body := h.get(h.newClient(), "/logs/live"); strings.Contains(body, "msg=boom") {
		t.Fatal("logs served without a session")
	}

	h.setup()
	resp, err := h.c.Get(h.srv.URL + "/logs/live?level=warn")
	if err != nil {
		t.Fatal(err)
	}
	data, _ := io.ReadAll(resp.Body)
	resp.Body.Close()
	if ct := resp.Header.Get("Content-Type"); ct != "text/plain; charset=utf-8" {
		t.Fatalf("content type %q", ct)
	}
	if cc := resp.Header.Get("Cache-Control"); cc != "no-store" {
		t.Fatalf("cache-control %q", cc)
	}
	if cd := resp.Header.Get("Content-Disposition"); cd != "" {
		t.Fatalf("the live view must not be a download: %q", cd)
	}
	got := string(data)
	if strings.Contains(got, "msg=hello") || !strings.Contains(got, "msg=careful") || !strings.Contains(got, "msg=boom") {
		t.Fatalf("warn filter: %q", got)
	}
	if strings.Count(got, "\n") != 2 || !strings.HasSuffix(got, "\n") {
		t.Fatalf("want one line per log line: %q", got)
	}

	// Default and unknown levels behave like the page: everything.
	for _, p := range []string{"/logs/live", "/logs/live?level=bogus"} {
		_, _, body := h.get(h.c, p)
		if !strings.Contains(body, "msg=hello") {
			t.Fatalf("%s did not default to debug: %q", p, body)
		}
	}
	// The same filter as the page it feeds.
	_, _, page := h.get(h.c, "/logs?level=warn")
	_, _, live := h.get(h.c, "/logs/live?level=warn")
	if !strings.Contains(page, "msg=boom") || strings.Contains(page, "msg=hello") || live != got {
		t.Fatal("page and live view disagree")
	}
}

func TestStaticAssetsAreRevalidated(t *testing.T) {
	h := newHarness(t)
	for _, p := range []string{"/static/app.js", "/static/style.css"} {
		resp, err := h.c.Get(h.srv.URL + p)
		if err != nil {
			t.Fatal(err)
		}
		resp.Body.Close()
		if cc := resp.Header.Get("Cache-Control"); !strings.Contains(cc, "no-cache") {
			t.Fatalf("%s cache-control %q", p, cc)
		}
		if resp.Header.Get("ETag") == "" || resp.Header.Get("Last-Modified") == "" {
			t.Fatalf("%s has no validator", p)
		}
	}
	// The layout asks for a versioned script, so an upgrade also busts caches.
	h.setup()
	_, _, body := h.get(h.c, "/logs")
	if !strings.Contains(body, `/static/app.js?v=test`) {
		t.Fatal("app.js is not referenced with the version")
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

func TestHealthzNeedsNoSession(t *testing.T) {
	h := newHarness(t)
	check := func(phase string) {
		t.Helper()
		resp, err := h.c.Get(h.srv.URL + "/healthz")
		if err != nil {
			t.Fatal(err)
		}
		body, _ := io.ReadAll(resp.Body)
		resp.Body.Close()
		if resp.StatusCode != http.StatusOK || string(body) != "ok\n" {
			t.Fatalf("%s: %d %q", phase, resp.StatusCode, body)
		}
	}
	// The probe must never redirect to /setup, before or after the password.
	check("setup mode")
	h.setup()
	check("password set")
}

func TestReplyLanguageSetting(t *testing.T) {
	h := newHarness(t)
	h.setup()
	if _, _, body := h.get(h.c, "/system"); !strings.Contains(body, `name="language"`) {
		t.Fatal("no language select on the System page")
	}
	h.post(h.c, "/system/language", url.Values{"language": {"fr"}})
	if got := h.cfg.Get().Language; got != "" {
		t.Fatalf("unsupported language saved: %q", got)
	}
	_, _, body := h.post(h.c, "/system/language", url.Values{"language": {"it"}})
	if got := h.cfg.Get().Language; got != "it" {
		t.Fatalf("language = %q, want it", got)
	}
	if !strings.Contains(body, `value="it" selected`) {
		t.Fatal("Italian not selected after saving")
	}
}

func TestPublicURLSetting(t *testing.T) {
	h := newHarness(t)
	h.setup()
	h.post(h.c, "/network/public-url", url.Values{"public_url": {"wa.example.com"}})
	if got := h.cfg.Get().PublicURL; got != "" {
		t.Fatalf("invalid address saved: %q", got)
	}
	h.post(h.c, "/network/public-url", url.Values{"public_url": {"https://wa.example.com"}})
	if got := h.cfg.Get().PublicURL; got != "https://wa.example.com" {
		t.Fatalf("public URL = %q", got)
	}
	if _, _, body := h.get(h.c, "/network"); !strings.Contains(body, `value="https://wa.example.com"`) {
		t.Fatal("saved address not shown in the form")
	}
	h.post(h.c, "/network/public-url", url.Values{"public_url": {""}})
	if got := h.cfg.Get().PublicURL; got != "" {
		t.Fatalf("address not cleared: %q", got)
	}
}

func TestPublicURLRefusedWhenNetworkIsManaged(t *testing.T) {
	h := newHarness(t)
	s := New(Options{
		Config: h.cfg, Status: h.st, Logs: h.logs, WA: h.wa,
		Getenv: func(string) string { return "" }, Version: "test",
		NetworkManaged: true,
	})
	req := httptest.NewRequest(http.MethodPost, "/network/public-url",
		strings.NewReader("public_url=https%3A%2F%2Fwa.example.com"))
	req.Header.Set("Content-Type", "application/x-www-form-urlencoded")
	rec := httptest.NewRecorder()
	s.networkPublicURL(rec, req)
	if got := h.cfg.Get().PublicURL; got != "" {
		t.Fatalf("address saved with network management on: %q", got)
	}
}
