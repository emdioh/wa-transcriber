package web

import (
	"context"
	"fmt"
	"net/http"
	"time"
)

// The System page's "Internal storage" card (M11, ARCHITECTURE.md section 11):
// the running SD card can be installed on the board's eMMC, so the device
// stops depending on a card that will eventually die.
//
// The page knows nothing about disks: main passes a FlashInstaller, and an
// implementation says which device it found (or why there is none), so a
// board without internal flash (the Raspberry Pi 2) shows one line instead of
// a button. With no FlashInstaller at all the card is absent and its route
// answers 404, like the two update cards.

// FlashStatus is a snapshot of the internal storage as the card shows it.
type FlashStatus struct {
	Enabled bool
	// Error is the one line that replaces the buttons: no internal storage
	// on this board, a target that cannot be told apart, no card layout.
	Error string
	// Target is the detected device ("/dev/mmcblk1") and Size its capacity;
	// Need is what the copy would write. Install is what the owner has to
	// type to confirm (the device name, like RESET for a factory reset).
	Target  string
	Size    int64
	Need    int64
	Install string
	Source  string // the running disk, "/dev/mmcblk0"
	// Marker is the file name on the boot partition that carries the /data
	// migration ("wat-migrate").
	Marker string
	// Busy is set while an install runs: the log shows the progress.
	Busy bool
	// ProgressDone and ProgressTotal are the bytes written so far and the
	// total work of this install: the copy is the first half, the read-back
	// the second, so 50 % means "written, now checking". Zero total when
	// nothing is running.
	ProgressDone  int64
	ProgressTotal int64
	// Done and DoneAt are the outcome of the last install in this process:
	// the internal storage is written and reads back identical. They exist
	// because the page that started the install is not the page that sees it
	// end - a phone locks its screen, a tab reloads, the WLAN drops - and the
	// owner has to be able to come back and read the result.
	Done   bool
	DoneAt time.Time
}

// FlashInstaller is what the card needs: the detected target, and the install
// that clones the card onto it, verifies it by read-back and writes the
// migration marker. Install never reboots: the owner powers the device off
// and removes the card.
type FlashInstaller interface {
	Status(ctx context.Context) FlashStatus
	Install(ctx context.Context, confirm string) (FlashStatus, error)
}

// flashInstallTimeout bounds one install: a 400 MB copy on a slow eMMC is
// minutes, plus the read-back, on the slowest board.
var flashInstallTimeout = 30 * time.Minute

// flashView is the card.
type flashView struct {
	Status FlashStatus
	Size   string // the target's capacity, "4 GB"
	Needs  string // what is copied onto it, "412 MB"
	// While an install runs: what it is doing and how far it got, so a page
	// loaded in the middle of one shows the state instead of the form.
	Phase   string // "copying", "checking the copy"
	Percent int
	Written string // "192 of 412 MB"
	// Refreshing tells the page to poll itself while the install runs.
	Refreshing bool
}

// flashPageView returns the card, or false when there is no installer.
func (s *Server) flashPageView(ctx context.Context) (*flashView, bool) {
	if s.opts.FlashInstaller == nil {
		return nil, false
	}
	st := s.opts.FlashInstaller.Status(ctx)
	if !st.Enabled {
		return nil, false
	}
	v := &flashView{Status: st, Size: diskText(st.Size), Needs: sizeText(st.Need)}
	if st.Busy && st.ProgressTotal > 0 {
		done := st.ProgressDone
		if done > st.ProgressTotal {
			done = st.ProgressTotal
		}
		v.Percent = int(done * 100 / st.ProgressTotal)
		v.Phase = "copying"
		written := done
		if done > st.Need {
			// Past the write: the clone is reading both sides back, which is
			// the half that says the eMMC really holds the bytes.
			v.Phase = "checking the copy"
			written = st.Need
		}
		v.Written = sizeText(written) + " of " + sizeText(st.Need)
	}
	v.Refreshing = st.Busy
	return v, true
}

// diskText is a disk capacity, rounded the way a vendor prints it: a 3.9 GB
// eMMC is "4 GB" on the box, and a copy that needs 412 MB fits it.
func diskText(n int64) string {
	const gb = 1 << 30
	if n >= gb {
		return fmt.Sprintf("%.0f GB", float64(n)/gb)
	}
	return sizeText(n)
}

// flashInstall runs the button. The confirmation is the device name, typed,
// like RESET in the factory reset card: one stray click must not write a disk.
func (s *Server) flashInstall(w http.ResponseWriter, r *http.Request) {
	v, ok := s.flashPageView(r.Context())
	if !ok {
		http.NotFound(w, r)
		return
	}
	if v.Status.Error != "" {
		s.back(w, r, "/system", false, "There is no internal storage to install to.")
		return
	}
	if v.Status.Busy {
		s.back(w, r, "/system", false, "An install is already running: watch the log.")
		return
	}
	confirm := r.PostFormValue("confirm")
	// The install must not die with the page. A phone that turns its screen
	// off, a reload or a dropped WLAN cancels the HTTP request, and this is a
	// disk write that takes minutes: detach the context from the request
	// (values are kept) and keep only the timeout, so losing the browser can
	// never leave a half-written internal storage behind. The card on /system
	// reports the outcome for whoever comes back, and the log has it either
	// way.
	ctx, cancel := context.WithTimeout(context.WithoutCancel(r.Context()), flashInstallTimeout)
	defer cancel()
	if _, err := s.opts.FlashInstaller.Install(ctx, confirm); err != nil {
		// This is the one failure the owner has to be told about in words:
		// the card still boots and the other disk is not bootable yet.
		s.opts.Log.Warn("install to the internal storage from the web UI failed", "err", err)
		s.back(w, r, "/system", false, "Install to the internal storage failed: "+err.Error()+
			". The internal storage is not bootable yet; this card still boots the device, so fix the "+
			"problem and run it again.")
		return
	}
	s.opts.Log.Warn("internal storage written and verified from the web UI", "target", v.Status.Target)
	s.back(w, r, "/system", true, "The internal storage is written and reads back identical. "+
		"Power the device off, remove the SD card and power it on again: it boots from the internal storage "+
		"and keeps its settings, WhatsApp pairing and WiFi networks.")
}
