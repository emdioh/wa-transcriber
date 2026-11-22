package imageupdate

import (
	"encoding/json"
	"errors"
	"fmt"
	"log/slog"
	"os"
	"path/filepath"
	"time"
)

// MaxBootTries is how many starts a freshly switched slot gets to confirm
// itself before Boot() rewrites the boot file back. Three is the M9 value
// (update.MaxTries): enough for a slow first boot and a power cut, few enough
// that a broken image costs a minute, not an evening.
const MaxBootTries = 3

// BootFile is the name of the state on /data. It lives next to config.json and
// session.db, which an image update never touches.
const BootFile = "image-boot.json"

// BootState is the slot bookkeeping on /data.
type BootState struct {
	Schema int `json:"schema"`
	// Pending is the slot of the last verified write. Install sets it, Switch
	// adds the image version, and it survives one start of the new slot that
	// has not confirmed yet — that is what the counter counts. It being set
	// does not mean the boot file names it: only Switch moves the boot file.
	Pending *BootSlot `json:"pending,omitempty"`
	// Previous is the slot that was running before the switch: the manual
	// rollback target.
	Previous *BootSlot `json:"previous,omitempty"`
	// Failure records the last automatic fallback, for the UI and the log.
	Failure *BootFailure `json:"failure,omitempty"`
}

// BootSlot is one slot as the state remembers it.
type BootSlot struct {
	Side string `json:"side"` // "A" or "B"
	Path string `json:"path"` // the partition device
	// Image is the version the installed image came from ("" unknown).
	Image string `json:"image,omitempty"`
	// SHA256 is the hash of the partition content as written and read back.
	SHA256 string `json:"sha256,omitempty"`
	// Tries counts starts of this slot since the switch without a confirm.
	Tries int `json:"tries"`
	// Confirmed is set by Confirm() 60 s into a healthy run.
	Confirmed bool `json:"confirmed"`
}

func bootStatePath(dataDir string) string { return filepath.Join(dataDir, BootFile) }

// LoadBoot reads image-boot.json. A missing file is not an error: it returns
// (nil, nil) and the caller starts from empty. A file that cannot be parsed is
// reported together with the empty state, exactly like update.LoadState: the
// device has to keep booting.
func LoadBoot(dataDir string) (*BootState, error) {
	if dataDir == "" {
		return nil, errors.New("imageupdate: no data directory")
	}
	b, err := os.ReadFile(bootStatePath(dataDir))
	if err != nil {
		if errors.Is(err, os.ErrNotExist) {
			return nil, nil
		}
		return nil, fmt.Errorf("imageupdate: read boot state: %w", err)
	}
	var st BootState
	if err := json.Unmarshal(b, &st); err != nil {
		return nil, fmt.Errorf("imageupdate: parse boot state: %w", err)
	}
	if st.Schema != 1 {
		return nil, fmt.Errorf("imageupdate: boot state schema %d, want 1", st.Schema)
	}
	return &st, nil
}

// SaveBoot writes image-boot.json atomically (temp, fsync, rename, fsync dir)
// so a power cut leaves the old or the new state, never a mixture.
func SaveBoot(dataDir string, st *BootState) error {
	if st == nil {
		return errors.New("imageupdate: save nil boot state")
	}
	if dataDir == "" {
		return errors.New("imageupdate: no data directory")
	}
	if st.Schema == 0 {
		st.Schema = 1
	}
	if st.Schema != 1 {
		return fmt.Errorf("imageupdate: cannot write boot state schema %d", st.Schema)
	}
	if err := os.MkdirAll(dataDir, 0o700); err != nil {
		return fmt.Errorf("imageupdate: create %s: %w", dataDir, err)
	}
	data, err := json.MarshalIndent(st, "", "  ")
	if err != nil {
		return fmt.Errorf("imageupdate: encode boot state: %w", err)
	}
	tmp, err := os.CreateTemp(dataDir, ".image-boot-*.json")
	if err != nil {
		return fmt.Errorf("imageupdate: create temp boot state: %w", err)
	}
	name := tmp.Name()
	fail := func(err error) error {
		tmp.Close()
		removeQuiet(name)
		return err
	}
	if err := tmp.Chmod(0o600); err != nil {
		return fail(fmt.Errorf("imageupdate: chmod temp boot state: %w", err))
	}
	if _, err := tmp.Write(append(data, '\n')); err != nil {
		return fail(fmt.Errorf("imageupdate: write temp boot state: %w", err))
	}
	if err := tmp.Sync(); err != nil {
		return fail(fmt.Errorf("imageupdate: fsync temp boot state: %w", err))
	}
	if err := tmp.Close(); err != nil {
		removeQuiet(name)
		return fmt.Errorf("imageupdate: close temp boot state: %w", err)
	}
	if err := os.Rename(name, bootStatePath(dataDir)); err != nil {
		removeQuiet(name)
		return fmt.Errorf("imageupdate: rename boot state: %w", err)
	}
	return SyncDir(dataDir)
}

// BootFailure is one automatic fallback, shown as a warning in the UI.
type BootFailure struct {
	Side   string    `json:"side"`
	Path   string    `json:"path"`
	Reason string    `json:"reason"`
	At     time.Time `json:"at"`
}

// BootAction is what Boot decided, mirroring update.Action: Note for the log,
// Reboot when the caller has to restart into the other slot.
type BootAction struct {
	Note   string
	Reboot bool
}

// Boot counts the start of a slot that has not confirmed itself and falls back
// after MaxBootTries. It is called at start-up from the same place as
// update.Boot, before the web UI exists, and only does something when the
// state says we owe a confirmation to a slot we switched to.
//
// This is the userspace half of the health gate: it catches an image that
// boots and starts this program but whose app never becomes healthy (a broken
// config migration, a crash loop, a full /data). An image whose kernel or init
// never reaches this program cannot be caught here — that counter lives in the
// bootloader (U-Boot's bootcount, a separate M10 step) and is out of scope for
// this package.
//
// Reboot is returned when the boot file was rewritten back to the previous
// slot and the caller must restart: the kernel already chose the broken root,
// so only a reboot gets to the other one.
func Boot(l Layout, now time.Time) (BootAction, error) { return BootIn(l, now, nil) }

// BootIn is Boot with the boot partition made writable for the fallback when
// the caller supplies bootRW (main does: /boot is mounted ro and mount(8) is
// the only way to change that). A nil bootRW writes directly, which is what a
// test with a temp directory wants.
func BootIn(l Layout, now time.Time, bootRW func() (func() error, error)) (BootAction, error) {
	st, err := LoadBoot(l.DataDir)
	if err != nil {
		return BootAction{}, err
	}
	if st == nil || st.Pending == nil {
		return BootAction{}, nil
	}
	slots, serr := l.FindSlots()
	if serr != nil {
		// No way to name the slots means no safe rollback: leave the state
		// alone and say so. A device that cannot find its slots has bigger
		// problems than the counter.
		return BootAction{}, serr
	}
	p := st.Pending
	if p.Confirmed || p.Side != slots.Current.Side {
		// Switched, confirmed or running the other slot: nothing to count.
		if !p.Confirmed {
			return BootAction{}, nil
		}
		return BootAction{Note: "image slot " + p.Side + " confirmed"}, nil
	}
	p.Tries++
	if p.Tries < MaxBootTries {
		if err := SaveBoot(l.DataDir, st); err != nil {
			return BootAction{}, err
		}
		return BootAction{Note: fmt.Sprintf("image slot %s start %d of %d, waiting for the health gate",
			p.Side, p.Tries, MaxBootTries)}, nil
	}

	// MaxBootTries starts without a confirmation: go back.
	reason := fmt.Sprintf("%d starts on root slot %s without becoming healthy", p.Tries, p.Side)
	log := slog.Default()
	action := BootAction{Note: "image rollback from " + p.Side + " to " + slots.Other.Side, Reboot: true}
	var unlock func() error
	if bootRW != nil {
		u, err := bootRW()
		if err != nil {
			// The slot is broken and the boot file cannot even be made
			// writable: keep the state so the next start tries again.
			return BootAction{}, errors.Join(fmt.Errorf("imageupdate: %s", reason), err)
		}
		if u != nil {
			unlock = u
		}
	}
	rerr := setRoot(l.BootFile, slots.Current.Path, slots.Other.Path)
	if unlock != nil {
		if uerr := unlock(); uerr != nil && rerr == nil {
			log.Warn("imageupdate: restoring the boot partition mount failed", "err", uerr)
		}
	}
	if rerr != nil {
		// The slot is broken and the boot file could not be moved: keep the
		// state so the next start tries again, and report both.
		return BootAction{}, errors.Join(fmt.Errorf("imageupdate: %s", reason), rerr)
	}
	st.Pending = nil
	st.Previous = &BootSlot{Side: slots.Current.Side, Path: slots.Current.Path, Tries: 0, Confirmed: false}
	st.Failure = &BootFailure{Side: p.Side, Path: p.Path, Reason: reason, At: now}
	if err := SaveBoot(l.DataDir, st); err != nil {
		// The boot file already points at the other slot, so the next start
		// is the good one whatever the state says.
		return action, err
	}
	log.Warn("imageupdate: falling back to the other root slot", "from", p.Side, "to", slots.Other.Side,
		"reason", reason)
	return action, nil
}
