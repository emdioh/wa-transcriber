package web

import (
	"context"
	"fmt"
	"net/http"
	"time"
)

// The System page shows the OS image (M10, D18) in a second card, next to the
// binary update. The page knows nothing about partitions or boot files: main
// passes a web.ImageUpdater, and an implementation can say "this layout has
// only one root" (LayoutError), which is the case the card exists for — an
// owner on an old image has to reflash once before image updates work.
//
// With no ImageUpdater (Docker, a dev machine, -image-update not set) the card
// is absent and its two routes answer 404.

// ImageStatus is a snapshot of the image state as the card shows it.
type ImageStatus struct {
	Enabled bool   // the feature is configured on this device
	Current string // running version, for the "the image you are on" line
	// LayoutError is set when image updates cannot work here: an old
	// single-root layout, a manifest that needs a newer partition table. The
	// card shows it instead of the buttons.
	LayoutError string
	// Ready says the two root slots could be told apart.
	Ready bool
	// Slot and OtherSlot are "A"/"B"; Bootable says the inactive slot holds
	// a verified image (false = unknown).
	Slot      string
	OtherSlot string
	Bootable  bool
	// Switched is set between the switch and the confirmation: the device
	// has to reboot (or has just rebooted) into the new slot.
	Switched bool
	// Available is the version of the released image this board could
	// install ("" when none), Installable whether it is newer.
	Available   string
	NotesURL    string
	ImageSize   int64
	Installable bool
	// CanRollback and RollbackTo drive "back to the previous image".
	CanRollback bool
	RollbackTo  string
	// LastFailure is the one-line record of an automatic fallback.
	LastFailure string
}

// ImageUpdater is what the System page needs from the image service. Install
// writes the image into the inactive slot and moves the boot file; Rollback
// moves it back. Both restart the device, so the handlers answer first.
type ImageUpdater interface {
	Status(ctx context.Context) ImageStatus
	Check(ctx context.Context) (ImageStatus, error)
	Install(ctx context.Context) (ImageStatus, error)
	Rollback(ctx context.Context) (ImageStatus, error)
}

// imageInstallTimeout bounds one install: a 200 MB image over WiFi on a Pi 2
// is minutes, plus the read-back.
var imageInstallTimeout = 20 * time.Minute

// imageView is the "System image" card.
type imageView struct {
	Status     ImageStatus
	Size       string // the image size, "184 MB"
	ReloadSecs int
}

// imagePageView returns the card, or false when there is no image service.
func (s *Server) imagePageView(ctx context.Context) (*imageView, bool) {
	if s.opts.ImageUpdater == nil {
		return nil, false
	}
	st := s.opts.ImageUpdater.Status(ctx)
	if !st.Enabled {
		return nil, false
	}
	return &imageView{Status: st, Size: sizeText(st.ImageSize), ReloadSecs: updateReloadSeconds}, true
}

// sizeText renders a byte count the way the cards do: "184 MB", "" for zero.
// The internal storage card uses it too, so both sizes read the same.
func sizeText(n int64) string {
	if n <= 0 {
		return ""
	}
	const mb = 1 << 20
	if n < mb {
		return fmt.Sprintf("%d kB", (n+1023)/1024)
	}
	return fmt.Sprintf("%.0f MB", float64(n)/mb)
}

// imageAction runs one button (name: check, install or rollback). Like
// updateAction it only re-checks that the feature is on and reports the
// result through the usual flash message.
func (s *Server) imageAction(w http.ResponseWriter, r *http.Request, what, name string) (ImageStatus, bool) {
	if _, ok := s.imagePageView(r.Context()); !ok {
		http.NotFound(w, r)
		return ImageStatus{}, false
	}
	timeout := updateActionTimeout
	if name == "install" {
		timeout = imageInstallTimeout
	}
	ctx, cancel := context.WithTimeout(r.Context(), timeout)
	defer cancel()
	var (
		st  ImageStatus
		err error
	)
	switch name {
	case "check":
		st, err = s.opts.ImageUpdater.Check(ctx)
	case "install":
		st, err = s.opts.ImageUpdater.Install(ctx)
	default:
		st, err = s.opts.ImageUpdater.Rollback(ctx)
	}
	if err != nil {
		s.opts.Log.Warn("image "+what+" from the web UI failed", "err", err)
		s.back(w, r, "/system", false, "Could not "+what+": "+err.Error())
		return st, false
	}
	return st, true
}

func (s *Server) imageCheck(w http.ResponseWriter, r *http.Request) {
	st, ok := s.imageAction(w, r, "check for an image", "check")
	if !ok {
		return
	}
	if st.LayoutError != "" {
		s.back(w, r, "/system", false, st.LayoutError)
		return
	}
	if st.Available == "" || !st.Installable {
		s.back(w, r, "/system", true, "No new image for this device.")
		return
	}
	s.back(w, r, "/system", true, "Image "+st.Available+" is available.")
}

func (s *Server) imageInstall(w http.ResponseWriter, r *http.Request) {
	if r.PostFormValue("confirm") != "yes" {
		s.back(w, r, "/system", false, "Tick the box to confirm the image update.")
		return
	}
	st, ok := s.imageAction(w, r, "install the image", "install")
	if !ok {
		return
	}
	s.opts.Log.Warn("image update installed from the web UI", "version", st.Available, "slot", st.OtherSlot)
	s.restartFlash(w, r, "Image "+versionName(st.Available)+
		" installed on the other slot, the device restarts into it in a moment.")
}

func (s *Server) imageRollback(w http.ResponseWriter, r *http.Request) {
	if r.PostFormValue("confirm") != "yes" {
		s.back(w, r, "/system", false, "Tick the box to confirm going back.")
		return
	}
	st, ok := s.imageAction(w, r, "go back to the previous image", "rollback")
	if !ok {
		return
	}
	s.opts.Log.Warn("image rollback requested from the web UI", "slot", st.RollbackTo)
	s.restartFlash(w, r, "Going back to the image on slot "+st.RollbackTo+
		", the device restarts in a moment.")
}
