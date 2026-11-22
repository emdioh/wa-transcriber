package imageupdate

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"net/http"
	"os"
	"sync"
	"time"
)

// The image update is a block device write, so every step is ordered so that
// the running slot and /data are never in a state that a power cut cannot
// explain:
//
//   Install  → write the inactive slot while hashing, fsync, read it back,
//              compare the hash. The boot file is not touched.
//   Switch   → one atomic rename in /boot naming the new slot, then the state
//              on /data that says who owes a confirmation.
//   Confirm  → 60 s of health: the slot keeps its hash and stops counting.
//   Boot     → 3 unconfirmed starts: the boot file goes back, reboot.
//
// A failure anywhere before Switch leaves the device running exactly what it
// runs now; a failure inside Switch leaves either the old or the new boot
// file; a failure after it is what the counter and the read-back are for.

// ErrAmbiguousSlots is returned when the two root slots cannot be told apart
// (see FindSlots). It is ErrNoSlots with the wording the UI uses.
var ErrAmbiguousSlots = ErrNoSlots

// Service writes images into the inactive root slot and moves the boot file.
// The paths are all in Layout, so a test runs the whole thing on files. One
// mutex serialises install, switch, rollback and confirm: two clicks cannot
// interleave two writes.
type Service struct {
	Layout Layout
	Log    *slog.Logger
	// Now is time.Now unless a test sets it.
	Now func() time.Time
	// VerifyRead replaces how the written partition is read back for the
	// sha256 comparison (a test injects a reader that returns wrong bytes).
	VerifyRead func(path string) (io.Reader, error)
	// HashDevice replaces how an existing partition is hashed (the health
	// gate compares the running slot with what was written).
	HashDevice func(path string) (string, error)
	// BootRW enters a writable boot partition and returns the way out. nil
	// means the caller mounted it rw already (a test, a dev machine).
	BootRW func() (func() error, error)

	mu sync.Mutex
}

func (s *Service) log() *slog.Logger {
	if s.Log != nil {
		return s.Log
	}
	return slog.Default()
}

func (s *Service) now() time.Time {
	if s.Now != nil {
		return s.Now()
	}
	return time.Now()
}

// WithBootRW is the Service version of the same thing: the injected helper
// when there is one, nothing otherwise.
func (s *Service) withBootRW() (func() error, error) {
	if s.BootRW == nil {
		return func() error { return nil }, nil
	}
	unlock, err := s.BootRW()
	if err != nil {
		return nil, err
	}
	if unlock == nil {
		unlock = func() error { return nil }
	}
	return unlock, nil
}

// Slots is the discovery the UI and main ask for, with the service's error
// wording.
func (s *Service) Slots() (Slots, error) { return s.Layout.FindSlots() }

// Install writes the image from src into the inactive root slot.
//
// src is read once; size and sha256 have to match the manifest, so a download
// that was truncated or tampered with is refused before the partition is
// touched. The write is streamed through a hasher, fsynced, and then the
// partition is read back and hashed again: a flash that silently wrote
// different bytes is caught here, not at the next boot.
//
// It never writes the running slot (FindSlots refuses when it cannot tell the
// two apart) and never writes /data. The caller flips the boot file with
// Switch after this returns.
func (s *Service) Install(ctx context.Context, src io.Reader, size int64, sum string) (SlotState, error) {
	var zero SlotState
	if src == nil {
		return zero, errors.New("imageupdate: install without a source")
	}
	if size <= 0 {
		return zero, fmt.Errorf("imageupdate: image size %d is not plausible", size)
	}
	if len(sum) != 64 || !isHex(sum) {
		return zero, fmt.Errorf("imageupdate: image sha256 %q", sum)
	}

	s.mu.Lock()
	defer s.mu.Unlock()

	slots, err := s.Layout.FindSlots()
	if err != nil {
		return zero, err
	}
	target := slots.Other
	// A sanity check that costs nothing and can only ever prevent a
	// catastrophe: the target must not be what / is mounted from.
	if target.Path == "" || target.Path == slots.Current.Path {
		return zero, ErrAmbiguousSlots
	}
	if target.Size > 0 && target.Size < size {
		return zero, fmt.Errorf("imageupdate: the other root slot is %d bytes, the image needs %d",
			target.Size, size)
	}

	f, err := os.OpenFile(target.Path, os.O_WRONLY, 0)
	if err != nil {
		return zero, fmt.Errorf("imageupdate: open %s: %w", target.Path, err)
	}
	h := sha256.New()
	n, copyErr := copyCtx(ctx, io.MultiWriter(f, h), src, size)
	if copyErr != nil {
		f.Close()
		return zero, fmt.Errorf("imageupdate: write %s: %w", target.Path, copyErr)
	}
	if n != size {
		f.Close()
		return zero, fmt.Errorf("imageupdate: wrote %d bytes into %s, the manifest says %d", n, target.Path, size)
	}
	if got := hex.EncodeToString(h.Sum(nil)); got != sum {
		f.Close()
		return zero, fmt.Errorf("imageupdate: the image has sha256 %s, the manifest says %s", got, sum)
	}
	if err := f.Sync(); err != nil {
		f.Close()
		return zero, fmt.Errorf("imageupdate: fsync %s: %w", target.Path, err)
	}
	if err := f.Close(); err != nil {
		return zero, fmt.Errorf("imageupdate: close %s: %w", target.Path, err)
	}

	// The read-back: what the partition returns now has to equal what we
	// meant to write. This is the one check that would catch an SD card
	// that lies, and it is cheap compared to the boot it saves.
	got, err := s.hashBack(target.Path)
	if err != nil {
		return zero, err
	}
	if got != sum {
		return zero, fmt.Errorf("imageupdate: %s reads back with sha256 %s, expected %s", target.Path, got, sum)
	}
	// The boot state says from here on that this slot holds a verified image,
	// so Switch can check it is still there and Confirm can compare the
	// partition under the kernel with it. It is not written to the boot file:
	// the device keeps running the slot it runs now.
	st, lerr := LoadBoot(s.Layout.DataDir)
	if lerr != nil && st == nil {
		return zero, lerr
	}
	if st == nil {
		st = &BootState{Schema: 1}
	}
	st.Pending = &BootSlot{Side: target.Side, Path: target.Path, SHA256: sum}
	if err := SaveBoot(s.Layout.DataDir, st); err != nil {
		return zero, err
	}
	s.log().Warn("imageupdate: image written and verified", "slot", target.Side, "device", target.Path,
		"bytes", size, "sha256", sum)
	return target, nil
}

// hashBack reads the written partition and returns its sha256.
func (s *Service) hashBack(path string) (string, error) {
	read := s.VerifyRead
	if read == nil {
		read = func(p string) (io.Reader, error) { return os.Open(p) }
	}
	r, err := read(path)
	if err != nil {
		return "", fmt.Errorf("imageupdate: read back %s: %w", path, err)
	}
	if c, ok := r.(io.Closer); ok {
		defer c.Close()
	}
	h := sha256.New()
	if _, err := io.Copy(h, r); err != nil {
		return "", fmt.Errorf("imageupdate: read back %s: %w", path, err)
	}
	return hex.EncodeToString(h.Sum(nil)), nil
}

// Switch moves the boot file to the other slot and records it in the boot
// state. image is the version string of what was written (for the UI, may be
// empty).
//
// The order is: state on /data first (it can be written without the boot
// partition being rw), then the boot file, then the state again to mark the
// pending slot. When the boot file write fails the state is put back, so
// nothing claims a switch that did not happen. The boot file itself is only
// ever replaced by an atomic rename.
func (s *Service) Switch(image string) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.switchLocked(image, true)
}

// switchLocked is Switch without the lock: rollback uses it too.
func (s *Service) switchLocked(image string, verify bool) error {
	slots, err := s.Layout.FindSlots()
	if err != nil {
		return err
	}
	var sum string
	if verify {
		// The slot has to still hold what Install wrote: switching to a
		// partition nobody checked is how an A/B system bricks itself.
		if sum, err = s.verifyOther(slots.Other); err != nil {
			return err
		}
	}
	st, err := LoadBoot(s.Layout.DataDir)
	if err != nil && st == nil {
		return err
	}
	if st == nil {
		st = &BootState{Schema: 1}
	}
	old := *st
	st.Pending = &BootSlot{Side: slots.Other.Side, Path: slots.Other.Path, Image: image, SHA256: sum}
	st.Previous = &BootSlot{Side: slots.Current.Side, Path: slots.Current.Path}
	if err := SaveBoot(s.Layout.DataDir, st); err != nil {
		return err
	}

	unlock, err := s.withBootRW()
	if err != nil {
		*st = old
		return errors.Join(err, SaveBoot(s.Layout.DataDir, st))
	}
	rerr := setRoot(s.Layout.BootFile, slots.Current.Path, slots.Other.Path)
	uerr := unlock()
	if rerr != nil {
		// The boot file is untouched (atomic rename): the running slot is
		// still the one that boots, and the state goes back to match.
		*st = old
		return errors.Join(rerr, uerr, SaveBoot(s.Layout.DataDir, st))
	}
	if uerr != nil {
		// Remounting back to ro failed. The switch itself is done; say so
		// but do not undo it.
		s.log().Warn("imageupdate: restoring the boot partition mount failed", "err", uerr)
	}
	s.log().Warn("imageupdate: boot file switched", "from", slots.Current.Side, "to", slots.Other.Side,
		"image", image, "boot_file", s.Layout.BootFile)
	return nil
}

// verifyOther is the check every Switch makes: the inactive slot has to still
// equal the image Install verified, or nobody has ever verified what is in
// it. It returns the hash to record as the pending slot's (taken from the
// verified write when there is one: hashing 2 GB again on a Pi 2 would cost
// minutes and prove nothing new).
func (s *Service) verifyOther(other SlotState) (string, error) {
	st, err := LoadBoot(s.Layout.DataDir)
	if err != nil && st == nil {
		return "", err
	}
	if st != nil && st.Pending != nil && st.Pending.Side == other.Side && st.Pending.SHA256 != "" {
		got, err := s.hashDevice(other.Path)
		if err != nil {
			return "", err
		}
		if got != st.Pending.SHA256 {
			return "", fmt.Errorf("imageupdate: %s no longer holds the verified image", other.Path)
		}
		return st.Pending.SHA256, nil
	}
	// No verified write to compare against. A Switch here would move the
	// boot file to a partition nobody read back: Switch only ever follows
	// Install in this program, so this is a bug, not a device state.
	return "", fmt.Errorf("imageupdate: %s was never written by a verified install", other.Path)
}

// hashDevice hashes a whole partition (the injected version when a test set
// one).
func (s *Service) hashDevice(path string) (string, error) {
	if s.HashDevice != nil {
		return s.HashDevice(path)
	}
	return HashDevice(path)
}

// HashDevice returns the sha256 of a block device or file.
func HashDevice(path string) (string, error) {
	f, err := os.Open(path)
	if err != nil {
		return "", fmt.Errorf("imageupdate: hash %s: %w", path, err)
	}
	defer f.Close()
	h := sha256.New()
	if _, err := io.Copy(h, f); err != nil {
		return "", fmt.Errorf("imageupdate: hash %s: %w", path, err)
	}
	return hex.EncodeToString(h.Sum(nil)), nil
}

// Rollback is the manual "back to the previous image": the boot file names the
// other slot again and the caller reboots. It refuses when the previous slot
// would be the one running now or when the boot file cannot be moved.
func (s *Service) Rollback() error {
	s.mu.Lock()
	defer s.mu.Unlock()
	slots, err := s.Layout.FindSlots()
	if err != nil {
		return err
	}
	st, err := LoadBoot(s.Layout.DataDir)
	if err != nil && st == nil {
		return err
	}
	if st == nil || st.Previous == nil || st.Previous.Side != slots.Other.Side {
		return errors.New("imageupdate: no previous image to go back to")
	}
	old := *st
	// The slot being left is the one that will run after a second rollback:
	// record it so the two buttons keep working in both directions.
	going := st.Pending
	st.Pending = &BootSlot{Side: slots.Other.Side, Path: slots.Other.Path}
	st.Previous = &BootSlot{Side: slots.Current.Side, Path: slots.Current.Path}
	if going != nil && going.Side == slots.Other.Side {
		st.Pending.SHA256 = going.SHA256
	}
	if err := SaveBoot(s.Layout.DataDir, st); err != nil {
		return err
	}
	unlock, err := s.withBootRW()
	if err != nil {
		*st = old
		return errors.Join(err, SaveBoot(s.Layout.DataDir, st))
	}
	rerr := setRoot(s.Layout.BootFile, slots.Current.Path, slots.Other.Path)
	uerr := unlock()
	if rerr != nil {
		*st = old
		return errors.Join(rerr, uerr, SaveBoot(s.Layout.DataDir, st))
	}
	if uerr != nil {
		s.log().Warn("imageupdate: restoring the boot partition mount failed", "err", uerr)
	}
	s.log().Warn("imageupdate: going back to the previous image", "from", slots.Current.Side, "to", slots.Other.Side)
	return nil
}

// Confirm is the Service version of the package function.
func (s *Service) Confirm() error {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.confirm()
}

// confirm is Confirm with the service's injected hash function.
func (s *Service) confirm() error {
	st, err := LoadBoot(s.Layout.DataDir)
	if err != nil && st == nil {
		return err
	}
	if st == nil || st.Pending == nil || st.Pending.Confirmed {
		return nil
	}
	slots, serr := s.Layout.FindSlots()
	if serr != nil {
		return serr
	}
	if st.Pending.Side != slots.Current.Side {
		return nil
	}
	sum, herr := s.hashDevice(slots.Current.Path)
	if herr != nil {
		return herr
	}
	p := st.Pending
	if p.SHA256 != "" && sum != p.SHA256 {
		return fmt.Errorf("imageupdate: root slot %s has sha256 %s, expected %s", p.Side, sum, p.SHA256)
	}
	p.SHA256 = sum
	p.Confirmed = true
	p.Tries = 0
	return SaveBoot(s.Layout.DataDir, st)
}

// Status is the snapshot the System page and the daily check read.
type Status struct {
	// Ready is false when the layout has no second root slot: the UI then
	// says "reflash once to enable image updates" and never downloads.
	Ready bool
	// Slot is the slot in use ("A"/"B"), empty when unknown.
	Slot string
	// OtherSlot is the inactive slot and Bootable says whether it holds a
	// verified image (or is unknown).
	OtherSlot string
	Bootable  bool
	// Switched is set when the boot file was moved and the new slot has not
	// confirmed yet (a reboot is pending or in progress).
	Switched bool
	// CanRollback and RollbackTo drive the "back to the previous image"
	// button.
	CanRollback bool
	RollbackTo  string
	// LastFailure is the one-line warning after an automatic fallback.
	LastFailure string
}

// Status reads the current state; it never fails the caller.
func (s *Service) Status() Status {
	s.mu.Lock()
	defer s.mu.Unlock()
	st := Status{}
	slots, err := s.Layout.FindSlots()
	if err != nil {
		return st
	}
	st.Ready = true
	st.Slot = slots.Current.Side
	st.OtherSlot = slots.Other.Side
	bs, berr := LoadBoot(s.Layout.DataDir)
	if berr != nil {
		s.log().Warn("imageupdate: reading the boot state", "err", berr)
	}
	if bs == nil {
		return st
	}
	if bs.Pending != nil {
		if bs.Pending.Side == slots.Other.Side {
			st.Bootable = bs.Pending.SHA256 != "" && !bs.Pending.Confirmed
			st.Switched = bs.Pending.Side != slots.Current.Side
		}
	}
	if bs.Previous != nil && bs.Previous.Side == slots.Other.Side {
		st.CanRollback = true
		st.RollbackTo = bs.Previous.Side
	}
	if f := bs.Failure; f != nil {
		st.LastFailure = fmt.Sprintf("root slot %s went back after %s", f.Side, f.Reason)
	}
	return st
}

// copyCtx copies exactly size bytes from r to w, stopping when ctx ends. It
// does not read past size, so a stream that is too long is caught by the
// caller's n comparison rather than silently written.
func copyCtx(ctx context.Context, w io.Writer, r io.Reader, size int64) (int64, error) {
	buf := make([]byte, 256<<10)
	var total int64
	for total < size {
		if err := ctx.Err(); err != nil {
			return total, err
		}
		want := int64(len(buf))
		if rem := size - total; rem < want {
			want = rem
		}
		n, err := r.Read(buf[:want])
		if n > 0 {
			if _, werr := w.Write(buf[:n]); werr != nil {
				return total, werr
			}
			total += int64(n)
		}
		if err != nil {
			if errors.Is(err, io.EOF) {
				return total, nil
			}
			return total, err
		}
	}
	return total, nil
}

// isHex reports whether s is all lowercase hex.
func isHex(s string) bool {
	for i := 0; i < len(s); i++ {
		c := s[i]
		if !(c >= '0' && c <= '9' || c >= 'a' && c <= 'f') {
			return false
		}
	}
	return true
}

// Downloader is the HTTP side: it streams a release asset into an image
// service without ever holding 200 MB in memory.
type Downloader struct {
	Client *http.Client
	// Slack is how far above the manifest size a response may grow before it
	// is refused (a skewed manifest, not a hostile server).
	Slack int64
}

// Fetch downloads url into svc.Install. The size and sha256 of the manifest
// are the ones Install checks, so a proxy that truncated the body is refused
// twice: by the socket and by the hash.
func (d *Downloader) Fetch(ctx context.Context, url string, svc *Service, size int64, sum string) (SlotState, error) {
	var zero SlotState
	if svc == nil {
		return zero, errors.New("imageupdate: download without a service")
	}
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, url, nil)
	if err != nil {
		return zero, fmt.Errorf("imageupdate: request: %w", err)
	}
	client := d.Client
	if client == nil {
		client = http.DefaultClient
	}
	resp, err := client.Do(req)
	if err != nil {
		return zero, fmt.Errorf("imageupdate: download: %w", err)
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		return zero, fmt.Errorf("imageupdate: download: HTTP %s", resp.Status)
	}
	slack := d.Slack
	if slack <= 0 {
		slack = 64 << 10
	}
	if resp.ContentLength > size+slack {
		return zero, fmt.Errorf("imageupdate: the server offers %d bytes, the manifest says %d", resp.ContentLength, size)
	}
	// LimitReader one byte past the cap: an over-long body is detected, not
	// written into a partition.
	body := io.LimitReader(resp.Body, size+1)
	slot, err := svc.Install(ctx, body, size, sum)
	if err != nil {
		return zero, err
	}
	return slot, nil
}

// FileSource is the io.Reader Install wants for a local image file, with its
// size (the release job's test path, and the manifest tool).
func FileSource(path string) (io.Reader, int64, error) {
	f, err := os.Open(path)
	if err != nil {
		return nil, 0, err
	}
	fi, err := f.Stat()
	if err != nil {
		f.Close()
		return nil, 0, err
	}
	return f, fi.Size(), nil
}
