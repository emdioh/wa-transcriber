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
	"github.com/emdioh/wa-transcriber/internal/status"
	"github.com/emdioh/wa-transcriber/internal/transcribe"
)

func (s *Server) routes() {
	m := s.mux
	m.Handle("GET /static/", http.FileServerFS(staticFS))
	m.HandleFunc("GET /setup", s.setupPage)
	m.HandleFunc("POST /setup", s.setupSubmit)
	m.HandleFunc("GET /login", s.loginPage)
	m.HandleFunc("POST /login", s.loginSubmit)
	m.HandleFunc("POST /logout", s.logout)

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
	m.HandleFunc("GET /logs", s.auth(s.logsPage))
	m.HandleFunc("GET /logs.txt", s.auth(s.logsText))
	m.HandleFunc("POST /logs/level", s.auth(s.logLevel))
	m.HandleFunc("GET /system", s.auth(s.systemPage))
	m.HandleFunc("POST /system/password", s.auth(s.changePassword))
	m.HandleFunc("POST /system/reset", s.auth(s.reset))
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
	Managed bool
	Addrs   []status.Addr
}

func (s *Server) networkPage(w http.ResponseWriter, r *http.Request) {
	v := networkView{Managed: s.opts.NetworkManaged}
	if s.opts.Addrs != nil {
		v.Addrs = s.opts.Addrs()
	}
	s.render(w, r, "network", "Network", v)
}

// -- logs -------------------------------------------------------------------

type logsView struct {
	Lines    []string
	Filter   string
	Total    int
	LogLevel string
	Levels   []string
}

var logLevels = []string{"debug", "info", "warn", "error"}

func (s *Server) logsPage(w http.ResponseWriter, r *http.Request) {
	filter := strings.ToLower(r.URL.Query().Get("level"))
	if !slices.Contains(logLevels, filter) {
		filter = "debug"
	}
	all := s.opts.Logs.Lines()
	v := logsView{Filter: filter, Total: len(all), LogLevel: s.logLevelName(), Levels: logLevels}
	min := levelRank(filter)
	for _, l := range all {
		if lineRank(l) >= min {
			v.Lines = append(v.Lines, l)
		}
	}
	s.render(w, r, "logs", "Logs", v)
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

func (s *Server) systemPage(w http.ResponseWriter, r *http.Request) {
	s.render(w, r, "system", "System", MinPasswordLen)
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
