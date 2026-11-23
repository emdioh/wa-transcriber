package web

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"io"
	"net/http"
	"strconv"
	"strings"
	"time"

	"github.com/emdioh/wa-transcriber/internal/backup"
)

// Backup and restore of the device state: the "Backup" card on the System
// page and the "restore a backup" section of the setup page. The archive
// itself is internal/backup's business; this file only moves the bytes and
// decides who may ask for them.
//
// The state is exactly what a reflash would otherwise cost: config.json (the
// password hash and the transcription API key), the WhatsApp session and the
// saved WiFi networks. So the download is behind s.auth, the upload is behind
// s.auth or — after a reflash, which leaves the device unclaimed — behind the
// setup-mode gate restoreAllowedByPasswordOnly, and neither route logs
// anything of what it carried.
//
// Both directions hold s.claiming for the duration: a restore rewrites
// config.json and the session, and a download must neither hand out half of a
// restore nor start on top of one. One at a time is enough for a device with
// one owner.

// backupView is the "Backup" card of the System page.
type backupView struct {
	// ReloadSecs is what the page says to wait after a restore: the restart
	// delay plus a margin for the new process to listen.
	ReloadSecs int
}

func (s *Server) backupPageView() *backupView {
	return &backupView{ReloadSecs: updateReloadSeconds}
}

// beginClaim serialises the routes that write or hand out the device state.
func (s *Server) beginClaim() func() {
	s.claiming.Lock()
	return s.claiming.Unlock
}

// restoreAllowedByPasswordOnly reports whether POST /restore may run because
// the device has no password yet.
//
// Why the setup page needs it: a reflash, a new card or the factory reset all
// leave config.json without a password, so the device comes up in setup mode
// — and the backup the owner needs is exactly the one that sets the password
// again. Demanding a session there would make the feature unusable at the
// only moment it exists for. Nothing is widened by it: an unclaimed device
// can already be claimed by whoever reaches it (that is what /setup does), so
// uploading a config.json with a password in it grants no more than typing
// the password does.
//
// The gate closes the moment a password exists — then the device is claimed
// and only a logged-in owner may replace its state — and it also closes while
// WhatsApp is linked, the same rule as the login page's reset
// (canResetWithoutPassword): a paired device is never touched without the
// password, whatever its config.json happens to say.
func (s *Server) restoreAllowedByPasswordOnly() bool {
	if s.opts.Config.Get().Web.PasswordHash != "" {
		return false
	}
	return s.opts.WA == nil || !s.opts.WA.Paired()
}

// downloadBackup sends the whole device state as an archive.
func (s *Server) downloadBackup(w http.ResponseWriter, r *http.Request) {
	if s.opts.DataDir == "" {
		http.NotFound(w, r)
		return
	}
	done := s.beginClaim()
	defer done()
	// Built in memory first: the count goes in the log, and a failure half
	// way must not look like a complete (short) download. The state is a few
	// hundred kB; backup.MaxArchive is the ceiling for a pathological one.
	var buf bytes.Buffer
	created := time.Now().UTC().Truncate(time.Second)
	meta, err := s.buildArchive(&buf, created)
	size := buf.Len()
	if err != nil {
		s.opts.Log.Error("building the backup failed", "err", err)
		http.Error(w, "could not read the device state", http.StatusInternalServerError)
		return
	}
	name := fmt.Sprintf("wa-transcriber-%s-%s.tar.gz",
		sanitizeName(firstNonEmpty(s.opts.Version, "device")), created.Format("20060102-150405"))
	h := w.Header()
	h.Set("Content-Type", "application/gzip")
	h.Set("Content-Disposition", `attachment; filename="`+name+`"`)
	// It holds the API key and the session: never in a cache.
	h.Set("Cache-Control", "no-store")
	h.Set("Content-Length", strconv.Itoa(size))
	if _, err := w.Write(buf.Bytes()); err != nil && r.Context().Err() == nil {
		s.opts.Log.Warn("sending the backup failed", "err", err)
	}
	// One line, and never any of the content: what left the device, how much
	// and how many files.
	s.opts.Log.Info("device state downloaded", "bytes", size,
		"files", strings.Join(meta.Names(), ","), "count", len(meta.Files))
}

// buildArchive writes the archive and checks it: an archive this device
// cannot restore itself is not one worth handing to the owner.
func (s *Server) buildArchive(buf *bytes.Buffer, created time.Time) (backup.Meta, error) {
	if err := backup.Write(buf, s.opts.DataDir, backup.Meta{Version: s.opts.Version, CreatedAt: created}); err != nil {
		return backup.Meta{}, err
	}
	return backup.Verify(bytes.NewReader(buf.Bytes()))
}

// restoreInstall puts an uploaded archive in place and restarts the device.
// The route has already decided that this request may do it.
func (s *Server) restoreInstall(w http.ResponseWriter, r *http.Request) {
	done := s.beginClaim()
	defer done()
	body, err := uploadBody(r)
	if err != nil {
		s.uploadError(w, err)
		return
	}
	meta, err := backup.Restore(body, s.opts.DataDir)
	if err != nil {
		// The reason, and no part of the archive: the message is the
		// package's own ("... does not match its checksum").
		s.opts.Log.Warn("restore of an uploaded archive refused", "err", err)
		s.uploadError(w, err)
		return
	}
	s.opts.Log.Warn("device state restored from an uploaded archive",
		"version", meta.Version, "created", meta.CreatedAt.Format(time.RFC3339), "count", len(meta.Files))
	// The restart is the point: the running process still holds the old
	// config and the old WhatsApp session in memory. restartPage answers,
	// flushes and only then hands off, so the message reaches the browser.
	s.restartPage(w, r, "Restoring the backup", "/", s.restoreRestart)
}

// restoreFromSetup is POST /restore: the archive is restored on a device that
// has no password yet, and the new config.json decides whether that is still
// true afterwards.
func (s *Server) restoreFromSetup(w http.ResponseWriter, r *http.Request) {
	if s.opts.DataDir == "" {
		http.NotFound(w, r)
		return
	}
	if !s.restoreAllowedByPasswordOnly() {
		if s.opts.Config.Get().Web.PasswordHash != "" {
			s.back(w, r, "/login", false,
				"This device has a password: log in and restore the backup from the System page.")
			return
		}
		s.back(w, r, "/setup", false,
			"The device is linked to WhatsApp: the backup can only be restored after logging in.")
		return
	}
	// Same serialisation as the authenticated route, taken before a byte is
	// read: a restore must not run beside the first password, nor beside
	// another upload.
	done := s.beginClaim()
	defer done()
	body, err := uploadBody(r)
	if err != nil {
		s.uploadError(w, err)
		return
	}
	meta, err := backup.Restore(body, s.opts.DataDir)
	if err != nil {
		s.opts.Log.Warn("restore on the setup page refused", "err", err)
		s.uploadError(w, err)
		return
	}
	s.opts.Log.Warn("device state restored on the setup page",
		"version", meta.Version, "created", meta.CreatedAt.Format(time.RFC3339), "count", len(meta.Files))
	// The archive's own config.json is now the device's: it usually has a
	// password, and either way the process must start again to read it and
	// to open the restored WhatsApp session.
	s.restartPage(w, r, "Restoring the backup", "/", s.restoreRestart)
}

// restoreRestart is the restart after a restore, through Options.Restart so
// that main decides what a restart is (the boot gate, the watchdog). Without
// one (a bare test server) there is nothing else to do: the files are in
// place and the next start picks them up.
func (s *Server) restoreRestart(context.Context) error {
	if s.opts.Restart != nil {
		s.opts.Restart()
	}
	return nil
}

// uploadError answers a refused or unreadable upload with the reason. It is
// the archive's own message, clipped: the client is the owner on the LAN and
// the alternative is "it did not work".
func (s *Server) uploadError(w http.ResponseWriter, err error) {
	msg := err.Error()
	if len(msg) > 300 {
		msg = msg[:300] + "…"
	}
	http.Error(w, "This file cannot be restored: "+msg, http.StatusBadRequest)
}

// maxUploadField bounds the multipart part in memory; the archive itself is
// capped by backup.MaxArchive.
const maxUploadField = 1 << 20

// uploadBody returns the uploaded archive: the multipart field "file" when
// there is one, the raw body otherwise (curl --data-binary @backup.tar.gz).
// Both forms are bounded by backup.MaxArchive, so a client cannot make the
// device read a stream for ever.
func uploadBody(r *http.Request) (io.Reader, error) {
	limit := backup.MaxArchive + 1
	if strings.HasPrefix(r.Header.Get("Content-Type"), "multipart/form-data") {
		if err := r.ParseMultipartForm(maxUploadField); err != nil {
			return nil, fmt.Errorf("the upload is not a readable form: %w", err)
		}
		f, _, err := r.FormFile("file")
		if err != nil {
			if errors.Is(err, http.ErrMissingFile) {
				return nil, errors.New("the form has no file in the field named file")
			}
			return nil, fmt.Errorf("the upload is not a readable form: %w", err)
		}
		// The handler closes nothing: the server closes the form's files.
		return io.LimitReader(f, int64(limit)), nil
	}
	return io.LimitReader(r.Body, int64(limit)), nil
}

// sanitizeName keeps a download name to what a filesystem accepts.
func sanitizeName(v string) string {
	var b strings.Builder
	for _, c := range v {
		switch {
		case c >= 'a' && c <= 'z', c >= 'A' && c <= 'Z', c >= '0' && c <= '9', c == '.', c == '-', c == '_':
			b.WriteRune(c)
		default:
			b.WriteByte('-')
		}
	}
	if b.Len() == 0 {
		return "device"
	}
	return b.String()
}
