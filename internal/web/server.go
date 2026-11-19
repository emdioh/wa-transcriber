// Package web is the device's configuration UI: plain server-rendered HTML
// embedded in the binary, protected by a password.
//
// Security model: one password (bcrypt hash in config.json). A login creates
// a random session token in an HttpOnly, SameSite=Strict cookie; sessions
// live in memory and are bound to the password hash, so any password change
// (web or !password) logs every session out. POSTs with a foreign Origin are
// refused. Until a password exists every page leads to /setup.
package web

import (
	"context"
	"crypto/rand"
	"crypto/subtle"
	"embed"
	"encoding/hex"
	"errors"
	"html/template"
	"io/fs"
	"log/slog"
	"net"
	"net/http"
	"net/url"
	"strings"
	"sync"
	"time"

	"go.mau.fi/whatsmeow/types"
	"golang.org/x/crypto/bcrypt"

	"github.com/emdioh/wa-transcriber/internal/config"
	"github.com/emdioh/wa-transcriber/internal/logbuf"
	"github.com/emdioh/wa-transcriber/internal/netmgr"
	"github.com/emdioh/wa-transcriber/internal/status"
)

//go:embed templates/*.html
var templateFS embed.FS

//go:embed static
var staticFS embed.FS

const (
	sessionCookie = "wat_session"
	flashCookie   = "wat_flash"
	sessionTTL    = 30 * 24 * time.Hour
	// MinPasswordLen matches the !password command.
	MinPasswordLen = 8
)

// loginFailDelay slows down password guessing (logins are serialised).
var loginFailDelay = time.Second

// WhatsApp is what the UI needs from the WhatsApp connection.
type WhatsApp interface {
	Paired() bool
	RequestPairingCode(ctx context.Context, phone string) error
	UseQR()
	ChatName(ctx context.Context, chat types.JID) string
	CanonicalChat(ctx context.Context, chat types.JID) types.JID
}

// Options wires the server to the rest of the program.
type Options struct {
	Config   *config.Store
	Status   *status.Tracker
	Logs     *logbuf.Buffer
	LogLevel *slog.LevelVar
	WA       WhatsApp
	Getenv   func(string) string // nil: os.Getenv
	// Unlink logs out of WhatsApp keeping the settings, then restarts.
	Unlink func(ctx context.Context) error
	// Reset erases everything and restarts (same as !reset).
	Reset   func(ctx context.Context) error
	Addrs   func() []status.Addr
	Version string
	Log     *slog.Logger
	// Network is the network manager; when nil the Network page falls back
	// to listing the addresses reported by Addrs.
	Network netmgr.Manager
	// NetworkManaged is false in Docker / on dev machines.
	NetworkManaged bool
	// InAPMode reports whether the setup access point is running. Captive
	// portal responses and the Host redirect are only active then.
	InAPMode func() bool
}

// Server serves the UI.
type Server struct {
	opts  Options
	mux   *http.ServeMux
	pages map[string]*template.Template

	mu       sync.Mutex
	sessions map[string]session
	loginMu  sync.Mutex
}

type session struct {
	expires time.Time
	pwHash  string // the password hash the session was created with
}

// New builds the server and its routes.
func New(opts Options) *Server {
	if opts.Log == nil {
		opts.Log = slog.Default()
	}
	s := &Server{opts: opts, mux: http.NewServeMux(), sessions: map[string]session{}}
	s.pages = parsePages()
	s.routes()
	return s
}

func parsePages() map[string]*template.Template {
	funcs := template.FuncMap{
		"ago":      agoText,
		"datetime": func(t time.Time) string { return t.Local().Format("2006-01-02 15:04:05") },
		"join":     strings.Join,
	}
	pages := map[string]*template.Template{}
	entries, err := fs.Glob(templateFS, "templates/*.html")
	if err != nil {
		panic(err)
	}
	for _, e := range entries {
		name := strings.TrimSuffix(strings.TrimPrefix(e, "templates/"), ".html")
		if name == "layout" {
			continue
		}
		pages[name] = template.Must(template.New("layout.html").Funcs(funcs).
			ParseFS(templateFS, "templates/layout.html", e))
	}
	return pages
}

// Handler returns the full handler chain.
func (s *Server) Handler() http.Handler {
	return securityHeaders(s.checkOrigin(s.captivePortal(s.mux)))
}

// ListenAndServe serves on addr until ctx ends.
func (s *Server) ListenAndServe(ctx context.Context, addr string) error {
	srv := &http.Server{
		Addr:              addr,
		Handler:           s.Handler(),
		ReadHeaderTimeout: 10 * time.Second,
		ReadTimeout:       30 * time.Second,
		WriteTimeout:      60 * time.Second,
		IdleTimeout:       2 * time.Minute,
		BaseContext:       func(net.Listener) context.Context { return ctx },
	}
	go func() {
		<-ctx.Done()
		shutdown, cancel := context.WithTimeout(context.Background(), 3*time.Second)
		defer cancel()
		_ = srv.Shutdown(shutdown)
	}()
	err := srv.ListenAndServe()
	if errors.Is(err, http.ErrServerClosed) {
		return nil
	}
	return err
}

// -- middleware -------------------------------------------------------------

func securityHeaders(next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		h := w.Header()
		h.Set("Content-Security-Policy",
			"default-src 'self'; img-src 'self' data:; style-src 'self'; script-src 'self'; "+
				"frame-ancestors 'none'; form-action 'self'; base-uri 'none'")
		h.Set("X-Content-Type-Options", "nosniff")
		h.Set("X-Frame-Options", "DENY")
		h.Set("Referrer-Policy", "same-origin")
		next.ServeHTTP(w, r)
	})
}

// checkOrigin refuses state-changing requests coming from another site
// (defence in depth on top of SameSite=Strict cookies).
func (s *Server) checkOrigin(next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Method != http.MethodGet && r.Method != http.MethodHead {
			src := r.Header.Get("Origin")
			if src == "" || src == "null" {
				src = r.Header.Get("Referer")
			}
			if src != "" {
				u, err := url.Parse(src)
				if err != nil || u.Host != r.Host {
					http.Error(w, "cross-site request refused", http.StatusForbidden)
					return
				}
			}
		}
		next.ServeHTTP(w, r)
	})
}

// auth wraps handlers that need a logged-in user.
func (s *Server) auth(h http.HandlerFunc) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		hash := s.opts.Config.Get().Web.PasswordHash
		if hash == "" {
			s.redirect(w, r, "/setup")
			return
		}
		if !s.validSession(r, hash) {
			if strings.HasPrefix(r.URL.Path, "/api/") || r.Method != http.MethodGet {
				http.Error(w, "login required", http.StatusUnauthorized)
				return
			}
			s.redirect(w, r, "/login?next="+url.QueryEscape(r.URL.RequestURI()))
			return
		}
		h(w, r)
	}
}

func (s *Server) validSession(r *http.Request, hash string) bool {
	c, err := r.Cookie(sessionCookie)
	if err != nil || c.Value == "" {
		return false
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	sess, ok := s.sessions[c.Value]
	if !ok {
		return false
	}
	if time.Now().After(sess.expires) || subtle.ConstantTimeCompare([]byte(sess.pwHash), []byte(hash)) != 1 {
		delete(s.sessions, c.Value)
		return false
	}
	return true
}

func (s *Server) startSession(w http.ResponseWriter, hash string) {
	var b [32]byte
	_, _ = rand.Read(b[:])
	token := hex.EncodeToString(b[:])
	now := time.Now()
	s.mu.Lock()
	for t, sess := range s.sessions { // opportunistic cleanup
		if now.After(sess.expires) || sess.pwHash != hash {
			delete(s.sessions, t)
		}
	}
	s.sessions[token] = session{expires: now.Add(sessionTTL), pwHash: hash}
	s.mu.Unlock()
	http.SetCookie(w, &http.Cookie{
		Name: sessionCookie, Value: token, Path: "/", MaxAge: int(sessionTTL.Seconds()),
		HttpOnly: true, SameSite: http.SameSiteStrictMode,
	})
}

func (s *Server) endSession(w http.ResponseWriter, r *http.Request) {
	if c, err := r.Cookie(sessionCookie); err == nil {
		s.mu.Lock()
		delete(s.sessions, c.Value)
		s.mu.Unlock()
	}
	http.SetCookie(w, &http.Cookie{Name: sessionCookie, Path: "/", MaxAge: -1, HttpOnly: true, SameSite: http.SameSiteStrictMode})
}

// setPassword stores a new hash and returns it.
func (s *Server) setPassword(pw string) (string, error) {
	hash, err := bcrypt.GenerateFromPassword([]byte(pw), bcrypt.DefaultCost)
	if err != nil {
		return "", err
	}
	if err := s.opts.Config.Update(func(c *config.Config) { c.Web.PasswordHash = string(hash) }); err != nil {
		return "", err
	}
	return string(hash), nil
}

// -- rendering --------------------------------------------------------------

type flash struct {
	OK   bool
	Text string
}

// pageData is what every template receives.
type pageData struct {
	Title    string
	Nav      string
	Version  string
	Flash    *flash
	LoggedIn bool
	Page     any
}

func (s *Server) render(w http.ResponseWriter, r *http.Request, name, title string, page any) {
	t, ok := s.pages[name]
	if !ok {
		http.Error(w, "unknown page", http.StatusInternalServerError)
		return
	}
	data := pageData{
		Title:    title,
		Nav:      name,
		Version:  s.opts.Version,
		Flash:    s.takeFlash(w, r),
		LoggedIn: name != "login" && name != "setup",
		Page:     page,
	}
	w.Header().Set("Content-Type", "text/html; charset=utf-8")
	w.Header().Set("Cache-Control", "no-store")
	if err := t.Execute(w, data); err != nil {
		s.opts.Log.Error("rendering page", "page", name, "err", err)
	}
}

func (s *Server) setFlash(w http.ResponseWriter, ok bool, text string) {
	v := "e:"
	if ok {
		v = "o:"
	}
	http.SetCookie(w, &http.Cookie{
		Name: flashCookie, Value: url.QueryEscape(v + text), Path: "/", MaxAge: 60,
		HttpOnly: true, SameSite: http.SameSiteStrictMode,
	})
}

func (s *Server) takeFlash(w http.ResponseWriter, r *http.Request) *flash {
	c, err := r.Cookie(flashCookie)
	if err != nil {
		return nil
	}
	http.SetCookie(w, &http.Cookie{Name: flashCookie, Path: "/", MaxAge: -1, HttpOnly: true, SameSite: http.SameSiteStrictMode})
	v, err := url.QueryUnescape(c.Value)
	if err != nil || len(v) < 2 {
		return nil
	}
	return &flash{OK: strings.HasPrefix(v, "o:"), Text: v[2:]}
}

// back redirects to path with a flash message (POST/redirect/GET).
func (s *Server) back(w http.ResponseWriter, r *http.Request, path string, ok bool, text string) {
	s.setFlash(w, ok, text)
	s.redirect(w, r, path)
}

func (s *Server) redirect(w http.ResponseWriter, r *http.Request, path string) {
	http.Redirect(w, r, path, http.StatusSeeOther)
}

func agoText(t time.Time) string {
	if t.IsZero() {
		return "never"
	}
	d := time.Since(t)
	switch {
	case d < time.Minute:
		return "just now"
	case d < time.Hour:
		return d.Round(time.Minute).String() + " ago"
	case d < 48*time.Hour:
		return d.Round(time.Hour).String() + " ago"
	}
	return t.Local().Format("2006-01-02 15:04")
}

// localPath accepts only same-site relative redirect targets.
func localPath(p string) string {
	if !strings.HasPrefix(p, "/") || strings.HasPrefix(p, "//") || strings.HasPrefix(p, "/\\") {
		return "/"
	}
	return p
}
