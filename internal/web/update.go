package web

import (
	"context"
	"net/http"
	"slices"
	"time"

	"github.com/emdioh/wa-transcriber/internal/config"
)

// The System page talks to the binary update service (M9, D17) through this
// interface, so the page knows nothing about GitHub, slots or state.json and
// the tests use a fake. main passes its implementation in Options.Updater;
// with no Updater (Docker, dev machines, a binary without the update key)
// the whole card is absent and the three routes answer 404.

// UpdateStatus is a snapshot of the update state as the page shows it.
type UpdateStatus struct {
	Enabled     bool      // the update directory is configured and a key is compiled in
	Current     string    // running version
	Available   string    // a newer release is known, "" when none
	NotesURL    string    // release notes of Available
	CheckedAt   time.Time // last successful check, zero when never
	Installed   string    // staged version waiting for the restart
	CanRollback bool
	RollbackTo  string // the version "back to" would run
	LastFailure string // "v0.2.0 was rolled back: 3 starts without becoming healthy"
}

// Updater is what the UI needs from the update service. Each action returns
// the status after it, so the page never has to guess what happened.
type Updater interface {
	Status(ctx context.Context) UpdateStatus
	Check(ctx context.Context) (UpdateStatus, error)
	Install(ctx context.Context) (UpdateStatus, error)
	Rollback(ctx context.Context) (UpdateStatus, error)
}

var (
	// updateRestartDelay lets the redirect and its flash cookie reach the
	// browser before the restart replaces the process (same value as
	// restartPage, which has the same problem).
	updateRestartDelay = 500 * time.Millisecond
	// updateActionTimeout bounds one button: a CDN check or a download.
	updateActionTimeout = 60 * time.Second
	// updateReloadSeconds is what the card tells the owner to wait: the
	// restart delay plus a generous margin for the new process to listen.
	updateReloadSeconds = int(updateRestartDelay/time.Second) + 10
)

// updateView is the "Software" card of the System page.
type updateView struct {
	Status UpdateStatus
	// Checked is the age of the last check ("checked 2h0m0s ago"), "" if never.
	Checked      string
	UpdateCheck  string // effective setting: off or daily
	UpdateAuto   bool   // effective setting: install without asking
	CheckEnv     string // variable overriding it, "" when config.json wins
	AutoEnv      string
	UpdateChecks []string
	ReloadSecs   int
}

// updateStatus asks the updater, when there is one and it is enabled.
func (s *Server) updateStatus(ctx context.Context) (UpdateStatus, bool) {
	if s.opts.Updater == nil {
		return UpdateStatus{}, false
	}
	st := s.opts.Updater.Status(ctx)
	return st, st.Enabled
}

func (s *Server) updatePageView(ctx context.Context) (*updateView, bool) {
	st, ok := s.updateStatus(ctx)
	if !ok {
		return nil, false
	}
	cfg := s.opts.Config.Get()
	env := config.EnvOverrides(s.opts.Getenv)
	v := &updateView{
		Status:       st,
		Checked:      checkedText(st.CheckedAt),
		UpdateCheck:  cfg.UpdateCheckMode(s.opts.Getenv),
		UpdateAuto:   cfg.UpdateAutoOn(s.opts.Getenv),
		CheckEnv:     env["update_check"],
		AutoEnv:      env["update_auto"],
		UpdateChecks: config.UpdateChecks,
		ReloadSecs:   updateReloadSeconds,
	}
	return v, true
}

// checkedText renders the age of the last check; empty when it never ran.
func checkedText(t time.Time) string {
	if t.IsZero() {
		return ""
	}
	return "checked " + agoText(t)
}

// updateAction runs one button of the card (name: check, install or
// rollback). The routes already carry s.auth, so this only re-checks that the
// feature is on (404 when off) and reports the result through the usual flash
// message. With no updater nothing is called: the page must not even name it.
func (s *Server) updateAction(w http.ResponseWriter, r *http.Request, what, name string) (UpdateStatus, bool) {
	if _, ok := s.updateStatus(r.Context()); !ok {
		http.NotFound(w, r)
		return UpdateStatus{}, false
	}
	ctx, cancel := context.WithTimeout(r.Context(), updateActionTimeout)
	defer cancel()
	var (
		st  UpdateStatus
		err error
	)
	switch name {
	case "check":
		st, err = s.opts.Updater.Check(ctx)
	case "install":
		st, err = s.opts.Updater.Install(ctx)
	default:
		st, err = s.opts.Updater.Rollback(ctx)
	}
	if err != nil {
		s.opts.Log.Warn("update "+what+" from the web UI failed", "err", err)
		s.back(w, r, "/system", false, "Could not "+what+": "+err.Error())
		return st, false
	}
	return st, true
}

func (s *Server) updateCheck(w http.ResponseWriter, r *http.Request) {
	st, ok := s.updateAction(w, r, "check for updates", "check")
	if !ok {
		return
	}
	if st.Available == "" {
		s.back(w, r, "/system", true, "No new version: "+versionName(st.Current)+" is the latest.")
		return
	}
	s.back(w, r, "/system", true, st.Available+" is available.")
}

func (s *Server) updateInstall(w http.ResponseWriter, r *http.Request) {
	if r.PostFormValue("confirm") != "yes" {
		s.back(w, r, "/system", false, "Tick the box to confirm the update.")
		return
	}
	st, ok := s.updateAction(w, r, "install the update", "install")
	if !ok {
		return
	}
	s.opts.Log.Warn("binary update installed from the web UI", "version", st.Installed)
	s.restartFlash(w, r, "Installing "+versionName(firstNonEmpty(st.Installed, st.Available))+
		", the device restarts in a moment.")
}

func (s *Server) updateRollback(w http.ResponseWriter, r *http.Request) {
	if r.PostFormValue("confirm") != "yes" {
		s.back(w, r, "/system", false, "Tick the box to confirm the rollback.")
		return
	}
	st, ok := s.updateAction(w, r, "roll back", "rollback")
	if !ok {
		return
	}
	s.opts.Log.Warn("binary rollback requested from the web UI", "version", st.RollbackTo)
	s.restartFlash(w, r, "Going back to "+versionName(st.RollbackTo)+
		", the device restarts in a moment.")
}

// updateSettings saves the two update settings. They are ordinary settings,
// not actions, so this reads like changeLanguage: validate, Update, flash.
// An environment override wins over config.json (D10), which the card shows.
func (s *Server) updateSettings(w http.ResponseWriter, r *http.Request) {
	if _, ok := s.updateStatus(r.Context()); !ok {
		http.NotFound(w, r)
		return
	}
	mode := r.PostFormValue("update_check")
	if !slices.Contains(config.UpdateChecks, mode) {
		s.back(w, r, "/system", false, "Unsupported update check mode.")
		return
	}
	auto := r.PostFormValue("update_auto") == "1"
	if err := s.opts.Config.Update(func(c *config.Config) {
		c.UpdateCheck = mode
		c.UpdateAuto = auto
	}); err != nil {
		s.opts.Log.Error("saving the update settings failed", "err", err)
		s.back(w, r, "/system", false, "Could not save the update settings.")
		return
	}
	s.opts.Log.Info("update settings changed from the web UI", "check", mode, "auto", auto)
	s.back(w, r, "/system", true, "Update settings saved.")
}

// restartFlash sends the redirect with its flash and only then restarts, in
// the background: syscall.Exec replaces the process, so nothing scheduled
// before the response is flushed could ever run. Flush pushes the 303 and the
// Set-Cookie to the browser now; the delay covers the follow-up request too.
func (s *Server) restartFlash(w http.ResponseWriter, r *http.Request, text string) {
	s.back(w, r, "/system", true, text)
	if f, ok := w.(http.Flusher); ok {
		f.Flush()
	}
	if s.opts.Restart == nil {
		return
	}
	go func() {
		time.Sleep(updateRestartDelay)
		s.opts.Restart()
	}()
}

// versionName renders a version for a message: "v0.3.0", or a phrase when
// the version is unknown.
func versionName(v string) string {
	if v == "" {
		return "the current version"
	}
	return v
}

func firstNonEmpty(values ...string) string {
	for _, v := range values {
		if v != "" {
			return v
		}
	}
	return ""
}
