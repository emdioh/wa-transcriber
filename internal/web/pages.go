package web

import (
	"context"
	"encoding/json"
	"fmt"
	"log/slog"
	"net/http"
	"net/url"
	"regexp"
	"runtime"
	"slices"
	"sort"
	"strconv"
	"strings"
	"time"

	"go.mau.fi/whatsmeow/types"
	"golang.org/x/crypto/bcrypt"

	"github.com/emdioh/wa-transcriber/internal/config"
	"github.com/emdioh/wa-transcriber/internal/netmgr"
	"github.com/emdioh/wa-transcriber/internal/status"
	"github.com/emdioh/wa-transcriber/internal/transcribe"
)

func (s *Server) routes() {
	m := s.mux
	m.Handle("GET /static/", staticHandler())
	m.HandleFunc("GET /setup", s.setupPage)
	m.HandleFunc("POST /setup", s.setupSubmit)
	m.HandleFunc("GET /login", s.loginPage)
	m.HandleFunc("POST /login", s.loginSubmit)
	m.HandleFunc("POST /logout", s.logout)
	m.HandleFunc("GET /healthz", s.healthz)

	m.HandleFunc("GET /{$}", s.auth(s.statusPage))
	m.HandleFunc("GET /api/status", s.auth(s.apiStatus))
	m.HandleFunc("GET /whatsapp", s.auth(s.whatsappPage))
	m.HandleFunc("GET /whatsapp/qr.svg", s.auth(s.qrSVG))
	m.HandleFunc("POST /whatsapp/phone", s.auth(s.pairPhone))
	m.HandleFunc("POST /whatsapp/qr", s.auth(s.pairQR))
	m.HandleFunc("POST /whatsapp/unlink", s.auth(s.unlink))
	m.HandleFunc("GET /transcription", s.auth(s.transcriptionPage))
	m.HandleFunc("POST /transcription", s.auth(s.transcriptionSubmit))
	m.HandleFunc("POST /transcription/chat", s.auth(s.chatSubmit))
	m.HandleFunc("GET /network", s.auth(s.networkPage))
	m.HandleFunc("POST /network/scan", s.auth(s.networkScan))
	m.HandleFunc("POST /network/connect", s.auth(s.networkConnect))
	m.HandleFunc("POST /network/forget", s.auth(s.networkForget))
	m.HandleFunc("POST /network/policy", s.auth(s.networkPolicy))
	m.HandleFunc("POST /network/public-url", s.auth(s.networkPublicURL))
	m.HandleFunc("GET /logs", s.auth(s.logsPage))
	m.HandleFunc("GET /logs.txt", s.auth(s.logsText))
	m.HandleFunc("GET /logs/live", s.auth(s.logsLive))
	m.HandleFunc("POST /logs/level", s.auth(s.logLevel))
	m.HandleFunc("GET /system", s.auth(s.systemPage))
	m.HandleFunc("POST /system/password", s.auth(s.changePassword))
	m.HandleFunc("POST /system/language", s.auth(s.changeLanguage))
	m.HandleFunc("POST /system/reset", s.auth(s.reset))
	// The update routes answer 404 when Options.Updater is nil, so a binary
	// without the feature gives nothing away.
	m.HandleFunc("POST /system/update/check", s.auth(s.updateCheck))
	m.HandleFunc("POST /system/update/install", s.auth(s.updateInstall))
	m.HandleFunc("POST /system/update/rollback", s.auth(s.updateRollback))
	m.HandleFunc("POST /system/update/settings", s.auth(s.updateSettings))
	// Same rule as the binary card: no ImageUpdater, no route.
	m.HandleFunc("POST /system/image/check", s.auth(s.imageCheck))
	m.HandleFunc("POST /system/image/install", s.auth(s.imageInstall))
	m.HandleFunc("POST /system/image/rollback", s.auth(s.imageRollback))
}

// -- setup / login ----------------------------------------------------------

func (s *Server) setupPage(w http.ResponseWriter, r *http.Request) {
	if s.opts.Config.Get().Web.PasswordHash != "" {
		s.redirect(w, r, "/login")
		return
	}
	s.render(w, r, "setup", "Welcome", MinPasswordLen)
}

func (s *Server) setupSubmit(w http.ResponseWriter, r *http.Request) {
	s.loginMu.Lock() // two first visitors must not both set a password
	defer s.loginMu.Unlock()
	if s.opts.Config.Get().Web.PasswordHash != "" {
		s.redirect(w, r, "/login")
		return
	}
	pw := r.PostFormValue("password")
	if msg := checkNewPassword(pw, r.PostFormValue("confirm")); msg != "" {
		s.back(w, r, "/setup", false, msg)
		return
	}
	hash, err := s.setPassword(pw)
	if err != nil {
		s.opts.Log.Error("saving the web password", "err", err)
		s.back(w, r, "/setup", false, "Could not save the password.")
		return
	}
	s.opts.Log.Info("web UI password set (first setup)")
	s.startSession(w, hash)
	s.back(w, r, "/whatsapp", true, "Password saved. Now link WhatsApp.")
}

func checkNewPassword(pw, confirm string) string {
	switch {
	case len([]rune(pw)) < MinPasswordLen:
		return fmt.Sprintf("The password needs at least %d characters.", MinPasswordLen)
	case pw != confirm:
		return "The two passwords differ."
	}
	return ""
}

func (s *Server) loginPage(w http.ResponseWriter, r *http.Request) {
	if s.opts.Config.Get().Web.PasswordHash == "" {
		s.redirect(w, r, "/setup")
		return
	}
	s.render(w, r, "login", "Log in", localPath(r.URL.Query().Get("next")))
}

func (s *Server) loginSubmit(w http.ResponseWriter, r *http.Request) {
	next := localPath(r.PostFormValue("next"))
	hash := s.opts.Config.Get().Web.PasswordHash
	if hash == "" {
		s.redirect(w, r, "/setup")
		return
	}
	// Serialised, with a delay after each failure: guessing stays slow even
	// with parallel requests.
	s.loginMu.Lock()
	err := bcrypt.CompareHashAndPassword([]byte(hash), []byte(r.PostFormValue("password")))
	if err != nil {
		time.Sleep(loginFailDelay)
	}
	s.loginMu.Unlock()
	if err != nil {
		s.opts.Log.Warn("web login failed", "remote", r.RemoteAddr)
		s.back(w, r, "/login?next="+url.QueryEscape(next), false, "Wrong password.")
		return
	}
	s.startSession(w, hash)
	s.redirect(w, r, next)
}

func (s *Server) logout(w http.ResponseWriter, r *http.Request) {
	s.endSession(w, r)
	s.redirect(w, r, "/login")
}

// -- status -----------------------------------------------------------------

type statusView struct {
	Snap          status.Snapshot
	Paired        bool
	Addrs         []status.Addr
	Transcription string
	Global        bool
	Overrides     int
	Uptime        string
	HeapMB        float64
	SysMB         float64
	Goroutines    int
	LogLevel      string
}

func (s *Server) statusPage(w http.ResponseWriter, r *http.Request) {
	cfg := s.opts.Config.Get()
	st := transcribe.Resolve(cfg.Transcription, s.opts.Getenv)
	desc := "not configured"
	if st.Configured() {
		desc = st.Provider + " / " + st.Model
		if st.TranslateTo != "" {
			desc += ", translate to " + st.TranslateTo
		}
	}
	var ms runtime.MemStats
	runtime.ReadMemStats(&ms)
	snap := s.opts.Status.Snapshot()
	v := statusView{
		Snap:          snap,
		Paired:        s.opts.WA.Paired(),
		Transcription: desc,
		Global:        cfg.Transcription.Global,
		Overrides:     len(s.chatRows(r.Context(), cfg.Transcription.Chats, false)),
		Uptime:        time.Since(snap.Started).Round(time.Second).String(),
		HeapMB:        float64(ms.HeapAlloc) / (1 << 20),
		SysMB:         float64(ms.Sys) / (1 << 20),
		Goroutines:    runtime.NumGoroutine(),
		LogLevel:      s.logLevelName(),
	}
	if s.opts.Addrs != nil {
		v.Addrs = s.opts.Addrs()
	}
	s.render(w, r, "status", "Status", v)
}

type apiStatus struct {
	Paired      bool   `json:"paired"`
	Connected   bool   `json:"connected"`
	Account     string `json:"account,omitempty"`
	Pairing     string `json:"pairing"` // changes whenever the QR/code on screen changes
	LastMessage string `json:"last_message,omitempty"`
}

// healthz is the container health probe: no session, no redirect, and it
// keeps working while the setup AP (captive portal) is running.
func (s *Server) healthz(w http.ResponseWriter, r *http.Request) {
	w.Header().Set("Content-Type", "text/plain; charset=utf-8")
	w.Header().Set("Cache-Control", "no-store")
	fmt.Fprintln(w, "ok")
}

func (s *Server) apiStatus(w http.ResponseWriter, r *http.Request) {
	snap := s.opts.Status.Snapshot()
	out := apiStatus{
		Paired:    s.opts.WA.Paired(),
		Connected: snap.Connected,
		Account:   snap.Account,
		Pairing:   snap.PairingCode,
	}
	if !snap.LastMessage.IsZero() {
		out.LastMessage = snap.LastMessage.UTC().Format(time.RFC3339)
	}
	w.Header().Set("Content-Type", "application/json")
	w.Header().Set("Cache-Control", "no-store")
	_ = json.NewEncoder(w).Encode(out)
}

// -- WhatsApp ---------------------------------------------------------------

type whatsappView struct {
	Paired    bool
	Connected bool
	Account   string
	Pairing   string // raw tracker value, for change detection
	QR        bool
	Code      string
	Stamp     int64 // cache buster for the QR image
}

func (s *Server) whatsappPage(w http.ResponseWriter, r *http.Request) {
	snap := s.opts.Status.Snapshot()
	v := whatsappView{
		Paired:    s.opts.WA.Paired(),
		Connected: snap.Connected,
		Account:   snap.Account,
		Pairing:   snap.PairingCode,
		Stamp:     time.Now().UnixNano(),
	}
	switch {
	case strings.HasPrefix(snap.PairingCode, "qr:"):
		v.QR = true
	case strings.HasPrefix(snap.PairingCode, "code:"):
		code := strings.TrimPrefix(snap.PairingCode, "code:")
		if len(code) == 8 {
			code = code[:4] + "-" + code[4:]
		}
		v.Code = code
	}
	s.render(w, r, "whatsapp", "WhatsApp", v)
}

func (s *Server) qrSVG(w http.ResponseWriter, r *http.Request) {
	code, ok := strings.CutPrefix(s.opts.Status.Snapshot().PairingCode, "qr:")
	if !ok {
		http.NotFound(w, r)
		return
	}
	svg, err := qrSVG(code)
	if err != nil {
		http.Error(w, err.Error(), http.StatusInternalServerError)
		return
	}
	w.Header().Set("Content-Type", "image/svg+xml")
	w.Header().Set("Cache-Control", "no-store")
	_, _ = w.Write(svg)
}

var phoneRE = regexp.MustCompile(`^\+?[0-9 ().-]{7,20}$`)

func (s *Server) pairPhone(w http.ResponseWriter, r *http.Request) {
	phone := strings.TrimSpace(r.PostFormValue("phone"))
	if !phoneRE.MatchString(phone) || strings.HasPrefix(strings.TrimPrefix(phone, "+"), "0") {
		s.back(w, r, "/whatsapp", false, "Enter the full number with country code, e.g. +39 333 1234567.")
		return
	}
	ctx, cancel := context.WithTimeout(r.Context(), 20*time.Second)
	defer cancel()
	if err := s.opts.WA.RequestPairingCode(ctx, phone); err != nil {
		s.back(w, r, "/whatsapp", false, "Could not get a pairing code: "+err.Error())
		return
	}
	s.back(w, r, "/whatsapp", true, "Code requested: enter it on the phone.")
}

func (s *Server) pairQR(w http.ResponseWriter, r *http.Request) {
	s.opts.WA.UseQR()
	s.back(w, r, "/whatsapp", true, "Back to the QR code: it appears with the next refresh (up to 20 seconds).")
}

func (s *Server) unlink(w http.ResponseWriter, r *http.Request) {
	if r.PostFormValue("confirm") != "yes" {
		s.back(w, r, "/whatsapp", false, "Tick the box to confirm.")
		return
	}
	s.opts.Log.Warn("unlink requested from the web UI")
	s.restartPage(w, r, "Unlinking WhatsApp", "/whatsapp", s.opts.Unlink)
}

// restartPage answers, then runs fn (which restarts the process) shortly
// after so the browser receives the page.
func (s *Server) restartPage(w http.ResponseWriter, r *http.Request, title, next string, fn func(context.Context) error) {
	s.render(w, r, "restart", title, next)
	if f, ok := w.(http.Flusher); ok {
		f.Flush()
	}
	if fn == nil {
		return
	}
	go func() {
		time.Sleep(500 * time.Millisecond)
		// Not r.Context(): this runs after the response, and the request
		// context is cancelled the moment the handler returns, which would
		// abort the logout and the re-exec before they happen.
		if err := fn(context.Background()); err != nil {
			s.opts.Log.Error(title+" failed", "err", err)
		}
	}()
}

// -- transcription ----------------------------------------------------------

type chatRow struct {
	Chat string   // canonical JID
	Keys []string // every config key of this chat
	Name string
	Who  string // "+39…", "group", …
	On   bool
}

type transcriptionView struct {
	Cfg       config.Transcription
	Eff       transcribe.Settings
	Env       map[string]string
	Providers []string
	Defaults  []providerDefault
	HasKey    bool
	Keep      string
	Chats     []chatRow
}

type providerDefault struct {
	Name, Model, TranslationModel string
}

func (s *Server) transcriptionPage(w http.ResponseWriter, r *http.Request) {
	cfg := s.opts.Config.Get().Transcription
	v := transcriptionView{
		Cfg:       cfg,
		Eff:       transcribe.Resolve(cfg, s.opts.Getenv),
		Env:       transcribe.EnvOverrides(s.opts.Getenv),
		Providers: transcribe.Providers,
		HasKey:    cfg.APIKey != "",
		Keep:      strings.Join(cfg.KeepLanguages, ", "),
		Chats:     s.chatRows(r.Context(), cfg.Chats, true),
	}
	for _, p := range transcribe.Providers {
		_, model, tm := transcribe.Defaults(p)
		if tm == "" {
			tm = "(same call)"
		}
		v.Defaults = append(v.Defaults, providerDefault{p, model, tm})
	}
	s.render(w, r, "transcription", "Transcription", v)
}

func (s *Server) transcriptionSubmit(w http.ResponseWriter, r *http.Request) {
	f := func(name string) string { return strings.TrimSpace(r.PostFormValue(name)) }
	provider := strings.ToLower(f("provider"))
	if !slices.Contains(transcribe.Providers, provider) {
		s.back(w, r, "/transcription", false, "Unknown provider.")
		return
	}
	var keep, bad []string
	for _, l := range strings.Split(f("keep_languages"), ",") {
		if strings.TrimSpace(l) == "" {
			continue
		}
		if code := transcribe.NormalizeLanguage(l); code != "" {
			if !slices.Contains(keep, code) {
				keep = append(keep, code)
			}
		} else {
			bad = append(bad, strings.TrimSpace(l))
		}
	}
	target := transcribe.NormalizeLanguage(f("translate_to"))
	if f("translate_to") != "" && target == "" {
		bad = append(bad, f("translate_to"))
	}
	if len(bad) > 0 {
		s.back(w, r, "/transcription", false,
			"Unknown language(s): "+strings.Join(bad, ", ")+". Use ISO codes like it, en, de.")
		return
	}
	timeout := 0
	if t := f("timeout_seconds"); t != "" {
		n, err := strconv.Atoi(t)
		if err != nil || n < 0 || n > 600 {
			s.back(w, r, "/transcription", false, "The timeout must be between 0 and 600 seconds.")
			return
		}
		timeout = n
	}
	baseURL := f("base_url")
	if baseURL != "" && !strings.HasPrefix(baseURL, "https://") && !strings.HasPrefix(baseURL, "http://") {
		s.back(w, r, "/transcription", false, "The base URL must start with https://")
		return
	}
	err := s.opts.Config.Update(func(c *config.Config) {
		t := &c.Transcription
		t.Global = r.PostFormValue("global") == "on"
		t.Provider = provider
		t.Model = f("model")
		t.BaseURL = baseURL
		t.TranslationModel = f("translation_model")
		t.KeepLanguages = keep
		t.TranslateTo = target
		t.TimeoutSeconds = timeout
		switch {
		case r.PostFormValue("clear_key") == "on":
			t.APIKey = ""
		case f("api_key") != "":
			t.APIKey = f("api_key")
		}
	})
	if err != nil {
		s.opts.Log.Error("saving transcription settings", "err", err)
		s.back(w, r, "/transcription", false, "Could not save the settings.")
		return
	}
	s.opts.Log.Info("transcription settings changed from the web UI", "provider", provider)
	s.back(w, r, "/transcription", true, "Settings saved. They apply to the next voice note.")
}

func (s *Server) chatSubmit(w http.ResponseWriter, r *http.Request) {
	if err := r.ParseForm(); err != nil {
		s.back(w, r, "/transcription", false, "Bad request.")
		return
	}
	keys := r.PostForm["key"]
	action := r.PostFormValue("action")
	if len(keys) == 0 || (action != "on" && action != "off" && action != "remove") {
		s.back(w, r, "/transcription", false, "Bad request.")
		return
	}
	err := s.opts.Config.Update(func(c *config.Config) {
		for _, k := range keys {
			if _, ok := c.Transcription.Chats[k]; !ok {
				continue // only existing entries can be changed here
			}
			if action == "remove" {
				delete(c.Transcription.Chats, k)
			} else {
				c.Transcription.Chats[k] = action == "on"
			}
		}
	})
	if err != nil {
		s.back(w, r, "/transcription", false, "Could not save the settings.")
		return
	}
	s.back(w, r, "/transcription#chats", true, "Chat setting updated.")
}

// chatRows groups the per-chat overrides by chat (a chat may be stored under
// its phone-number and LID form). names: also look up display names.
func (s *Server) chatRows(ctx context.Context, chats map[string]bool, names bool) []chatRow {
	ctx, cancel := context.WithTimeout(ctx, 3*time.Second)
	defer cancel()
	byChat := map[string]*chatRow{}
	for key, on := range chats {
		canonical := key
		jid, err := types.ParseJID(key)
		if err == nil {
			canonical = s.opts.WA.CanonicalChat(ctx, jid).String()
		}
		row, ok := byChat[canonical]
		if !ok {
			row = &chatRow{Chat: canonical, On: on, Who: describeJID(canonical)}
			byChat[canonical] = row
		}
		row.Keys = append(row.Keys, key)
		if key == canonical {
			row.On = on
		}
	}
	rows := make([]chatRow, 0, len(byChat))
	for _, row := range byChat {
		sort.Strings(row.Keys)
		if names {
			if jid, err := types.ParseJID(row.Chat); err == nil {
				row.Name = s.opts.WA.ChatName(ctx, jid)
			}
		}
		rows = append(rows, *row)
	}
	sort.Slice(rows, func(i, j int) bool {
		a, b := strings.ToLower(rows[i].Name+rows[i].Chat), strings.ToLower(rows[j].Name+rows[j].Chat)
		return a < b
	})
	return rows
}

func describeJID(s string) string {
	jid, err := types.ParseJID(s)
	if err != nil {
		return s
	}
	switch jid.Server {
	case types.DefaultUserServer:
		return "+" + jid.User
	case types.GroupServer:
		return "group"
	case types.HiddenUserServer:
		return "contact (hidden number)"
	}
	return s
}

// -- network ----------------------------------------------------------------

type networkView struct {
	Managed    bool
	InAPMode   bool
	Interfaces []netmgr.Interface
	Scan       []netmgr.Network
	ScanAge    string
	Saved      []netmgr.SavedNetwork
	Policy     string
	Policies   []string
	PublicURL  string
}

func (s *Server) networkPage(w http.ResponseWriter, r *http.Request) {
	v := networkView{
		Managed: s.opts.NetworkManaged, Policies: []string{"auto", "always", "never"}, Policy: "auto",
		PublicURL: s.opts.Config.Get().PublicURL,
	}
	switch {
	case s.opts.Network != nil:
		v.Interfaces = s.opts.Network.Status().Interfaces
		v.InAPMode = s.opts.Network.InAPMode()
		v.Saved = s.opts.Network.SavedNetworks()
		v.Policy = string(s.opts.Network.APPolicy())
	case s.opts.Addrs != nil:
		v.Interfaces = netmgr.StatusFromAddrs(s.opts.Addrs()).Interfaces
	}
	s.mu.Lock()
	if time.Since(s.lastScan) < 2*time.Minute {
		v.Scan = s.lastScanResult
		v.ScanAge = agoText(s.lastScan)
	}
	s.mu.Unlock()
	s.render(w, r, "network", "Network", v)
}

func (s *Server) networkScan(w http.ResponseWriter, r *http.Request) {
	if s.opts.Network == nil {
		s.back(w, r, "/network", false, "Network management is off.")
		return
	}
	ctx, cancel := context.WithTimeout(r.Context(), 20*time.Second)
	defer cancel()
	nets, err := s.opts.Network.Scan(ctx)
	if err != nil {
		s.back(w, r, "/network", false, "Scan failed: "+err.Error())
		return
	}
	s.mu.Lock()
	s.lastScanResult = nets
	s.lastScan = time.Now()
	s.mu.Unlock()
	s.back(w, r, "/network", true, fmt.Sprintf("%d networks found.", len(nets)))
}

func (s *Server) networkConnect(w http.ResponseWriter, r *http.Request) {
	if s.opts.Network == nil {
		s.back(w, r, "/network", false, "Network management is off.")
		return
	}
	ssid := strings.TrimSpace(r.PostFormValue("ssid"))
	pass := r.PostFormValue("passphrase")
	if ssid == "" {
		s.back(w, r, "/network", false, "Choose a network.")
		return
	}
	if len(pass) < 8 && r.PostFormValue("secured") == "1" {
		s.back(w, r, "/network", false, "The passphrase needs at least 8 characters.")
		return
	}
	wasAP := s.opts.Network.InAPMode()
	if err := s.opts.Network.SaveNetwork(ssid, pass); err != nil {
		s.back(w, r, "/network", false, "Could not save the network: "+err.Error())
		return
	}
	s.opts.Log.Info("WiFi network saved from the web UI", "ssid", ssid, "in_ap_mode", wasAP)
	if wasAP {
		s.render(w, r, "restart", "Switching network",
			"The device is switching network: reconnect your phone to "+ssid+" and open http://wa.local/")
		return
	}
	s.back(w, r, "/network", true, "Saved. The device will join "+ssid+" within a minute.")
}

func (s *Server) networkForget(w http.ResponseWriter, r *http.Request) {
	if s.opts.Network == nil {
		s.back(w, r, "/network", false, "Network management is off.")
		return
	}
	ssid := strings.TrimSpace(r.PostFormValue("ssid"))
	if err := s.opts.Network.ForgetNetwork(ssid); err != nil {
		s.back(w, r, "/network", false, "Could not forget the network: "+err.Error())
		return
	}
	s.back(w, r, "/network", true, "Forgot "+ssid+".")
}

// networkPublicURL saves the address people use to reach the UI when the
// device cannot know it (Docker, a dev machine). !status lists it first.
func (s *Server) networkPublicURL(w http.ResponseWriter, r *http.Request) {
	if s.opts.NetworkManaged {
		s.back(w, r, "/network", false, "The device finds its own address.")
		return
	}
	raw := strings.TrimSpace(r.PostFormValue("public_url"))
	if raw != "" && !validPublicURL(raw) {
		s.back(w, r, "/network", false, "The address must start with http:// or https://.")
		return
	}
	if err := s.opts.Config.Update(func(c *config.Config) { c.PublicURL = raw }); err != nil {
		s.opts.Log.Error("saving the public URL failed", "err", err)
		s.back(w, r, "/network", false, "Could not save the address.")
		return
	}
	s.opts.Log.Info("public URL changed from the web UI", "set", raw != "")
	s.back(w, r, "/network", true, "Address saved.")
}

// validPublicURL accepts an absolute http(s) URL with a host.
func validPublicURL(raw string) bool {
	u, err := url.Parse(raw)
	if err != nil || (u.Scheme != "http" && u.Scheme != "https") || u.Host == "" {
		return false
	}
	return true
}

func (s *Server) networkPolicy(w http.ResponseWriter, r *http.Request) {
	if s.opts.Network == nil {
		s.back(w, r, "/network", false, "Network management is off.")
		return
	}
	p := netmgr.APPolicy(strings.ToLower(r.PostFormValue("policy")))
	if err := s.opts.Network.SetAPPolicy(p); err != nil {
		s.back(w, r, "/network", false, "Unknown policy.")
		return
	}
	s.back(w, r, "/network", true, "Setup access point: "+string(p)+".")
}

// -- logs -------------------------------------------------------------------

type logsView struct {
	Lines    []string
	Filter   string
	Total    int
	LogLevel string
	Levels   []string
	Updated  string // HH:MM:SS of the render, shown next to the line count
}

var logLevels = []string{"debug", "info", "warn", "error"}

func (s *Server) logsPage(w http.ResponseWriter, r *http.Request) {
	filter := logFilter(r)
	all := s.opts.Logs.Lines()
	v := logsView{
		Lines: filteredLines(all, filter), Filter: filter, Total: len(all),
		LogLevel: s.logLevelName(), Levels: logLevels, Updated: time.Now().Format("15:04:05"),
	}
	s.render(w, r, "logs", "Logs", v)
}

// logsLive is what the Logs page polls: the same lines logsPage shows, as
// plain text, so the page can refresh without reloading.
func (s *Server) logsLive(w http.ResponseWriter, r *http.Request) {
	w.Header().Set("Content-Type", "text/plain; charset=utf-8")
	w.Header().Set("Cache-Control", "no-store")
	for _, l := range filteredLines(s.opts.Logs.Lines(), logFilter(r)) {
		_, _ = w.Write([]byte(l + "\n"))
	}
}

// logFilter reads ?level=, defaulting to everything (debug).
func logFilter(r *http.Request) string {
	filter := strings.ToLower(r.URL.Query().Get("level"))
	if !slices.Contains(logLevels, filter) {
		return "debug"
	}
	return filter
}

// filteredLines keeps the lines at or above the rank of filter.
func filteredLines(all []string, filter string) []string {
	var out []string
	min := levelRank(filter)
	for _, l := range all {
		if lineRank(l) >= min {
			out = append(out, l)
		}
	}
	return out
}

func (s *Server) logsText(w http.ResponseWriter, r *http.Request) {
	w.Header().Set("Content-Type", "text/plain; charset=utf-8")
	w.Header().Set("Content-Disposition",
		`attachment; filename="wa-transcriber-`+time.Now().Format("20060102-150405")+`.log"`)
	for _, l := range s.opts.Logs.Lines() {
		_, _ = w.Write([]byte(l + "\n"))
	}
}

func (s *Server) logLevel(w http.ResponseWriter, r *http.Request) {
	name := strings.ToLower(r.PostFormValue("level"))
	var lvl slog.Level
	if !slices.Contains(logLevels, name) || lvl.UnmarshalText([]byte(name)) != nil || s.opts.LogLevel == nil {
		s.back(w, r, "/logs", false, "Unknown level.")
		return
	}
	s.opts.LogLevel.Set(lvl)
	s.opts.Log.Info("log level changed from the web UI", "level", name)
	s.back(w, r, "/logs", true, "Log level set to "+name+" (until the next restart).")
}

func (s *Server) logLevelName() string {
	if s.opts.LogLevel == nil {
		return "info"
	}
	return strings.ToLower(s.opts.LogLevel.Level().String())
}

func levelRank(name string) int { return slices.Index(logLevels, name) }

// lineRank reads "level=WARN" from a slog text line (-1 if absent).
func lineRank(line string) int {
	_, rest, ok := strings.Cut(line, " level=")
	if !ok {
		return -1
	}
	lvl, _, _ := strings.Cut(rest, " ")
	return levelRank(strings.ToLower(lvl))
}

// -- system -----------------------------------------------------------------

type systemView struct {
	MinPasswordLen int
	Languages      []string
	Language       string
	// Update is nil when the binary update feature is off: no card at all.
	Update *updateView
	// Image is nil when the image update feature is off (M10).
	Image *imageView
}

func (s *Server) systemPage(w http.ResponseWriter, r *http.Request) {
	update, _ := s.updatePageView(r.Context())
	image, _ := s.imagePageView(r.Context())
	s.render(w, r, "system", "System", systemView{
		MinPasswordLen: MinPasswordLen,
		Languages:      config.Languages,
		Language:       config.NormalizeLanguage(s.opts.Config.Get().Language),
		Update:         update,
		Image:          image,
	})
}

// changeLanguage sets the language of the bot's WhatsApp replies.
func (s *Server) changeLanguage(w http.ResponseWriter, r *http.Request) {
	lang := r.PostFormValue("language")
	if !slices.Contains(config.Languages, lang) {
		s.back(w, r, "/system", false, "Unsupported language.")
		return
	}
	if err := s.opts.Config.Update(func(c *config.Config) { c.Language = lang }); err != nil {
		s.opts.Log.Error("saving the reply language failed", "err", err)
		s.back(w, r, "/system", false, "Could not save the language.")
		return
	}
	s.opts.Log.Info("bot reply language changed from the web UI", "language", lang)
	s.back(w, r, "/system", true, "Language saved.")
}

func (s *Server) changePassword(w http.ResponseWriter, r *http.Request) {
	hash := s.opts.Config.Get().Web.PasswordHash
	s.loginMu.Lock()
	err := bcrypt.CompareHashAndPassword([]byte(hash), []byte(r.PostFormValue("current")))
	if err != nil {
		time.Sleep(loginFailDelay)
	}
	s.loginMu.Unlock()
	if err != nil {
		s.back(w, r, "/system", false, "The current password is wrong.")
		return
	}
	pw := r.PostFormValue("password")
	if msg := checkNewPassword(pw, r.PostFormValue("confirm")); msg != "" {
		s.back(w, r, "/system", false, msg)
		return
	}
	newHash, err := s.setPassword(pw)
	if err != nil {
		s.back(w, r, "/system", false, "Could not save the password.")
		return
	}
	s.opts.Log.Info("web UI password changed")
	s.startSession(w, newHash) // every other session is now invalid
	s.back(w, r, "/system", true, "Password changed. Other browsers have been logged out.")
}

func (s *Server) reset(w http.ResponseWriter, r *http.Request) {
	if r.PostFormValue("confirm") != "RESET" {
		s.back(w, r, "/system", false, `Type RESET to confirm.`)
		return
	}
	s.opts.Log.Warn("factory reset requested from the web UI")
	s.endSession(w, r)
	s.restartPage(w, r, "Factory reset", "/setup", s.opts.Reset)
}
