package update

import (
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"time"
)

// MaxTries is how many starts an unverified active version gets before Boot
// rolls it back. The running process marks itself verified after 60 s of
// uptime (in main, not here), so a version that dies immediately never gets
// that far.
const MaxTries = 3

// Slot is one binary on /data/bin. File is the absolute path, SHA256 the
// hash recorded at install (empty only for hand-written or adopted state,
// where the file's existence is enough).
type Slot struct {
	Version string `json:"version"`
	File    string `json:"file"`
	SHA256  string `json:"sha256,omitempty"`
}

// Failure records why a version was rolled back, for the System page.
type Failure struct {
	Version string    `json:"version"`
	Reason  string    `json:"reason"`
	At      time.Time `json:"at"`
}

// State is state.json: the three slots plus the try counter of the active
// version. Active/Previous/Pending are nil when empty.
type State struct {
	Schema   int   `json:"schema"`
	Active   *Slot `json:"active,omitempty"`
	Previous *Slot `json:"previous,omitempty"`
	Pending  *Slot `json:"pending,omitempty"`
	Tries    int   `json:"tries"`
	// Verified is set by the caller once the active version has been up long
	// enough to count as healthy. A verified active is never counted.
	Verified    bool     `json:"verified"`
	LastFailure *Failure `json:"last_failure,omitempty"`
}

// StateFile is the name of the state file inside the update directory.
const StateFile = "state.json"

func statePath(dir string) string { return filepath.Join(dir, StateFile) }

// binaryPrefix is the name prefix of every managed binary:
// wa-transcriber-<version>.
const binaryPrefix = "wa-transcriber-"

// BinaryName is the file name of the binary for version in dir.
func BinaryName(dir, version string) string {
	return filepath.Join(dir, binaryPrefix+version)
}

// LoadState reads state.json from dir. A missing file is a fresh card and
// gives a valid empty state; an unreadable or unparsable file is moved aside
// to state.json.bad and also gives an empty state. It never fails a boot
// because of bad content — only an error when the file cannot even be
// renamed, and even that is reported along with the empty state.
func LoadState(dir string) (*State, error) {
	st := &State{Schema: 1}
	p := statePath(dir)
	b, err := os.ReadFile(p)
	if err != nil {
		if isNotExist(err) {
			return st, nil
		}
		return st, fmt.Errorf("update: read %s: %w", StateFile, err)
	}
	var loaded State
	if err := json.Unmarshal(b, &loaded); err != nil {
		// Bad JSON, empty file after a power cut, or a schema from the
		// future: keep it for diagnosis, start empty.
		bad := p + ".bad"
		_ = os.Remove(bad)
		if rerr := os.Rename(p, bad); rerr != nil {
			return st, fmt.Errorf("update: %s is unreadable (%v) and cannot be moved aside: %w", StateFile, err, rerr)
		}
		return st, nil
	}
	if loaded.Schema != 0 && loaded.Schema != 1 {
		bad := p + ".bad"
		_ = os.Remove(bad)
		_ = os.Rename(p, bad)
		return st, nil
	}
	loaded.Schema = 1
	loaded.sanitize(dir)
	return &loaded, nil
}

// sanitize repairs what a hand-edited or half-written state could contain:
// a slot whose file name is not inside dir, a negative counter, a slot
// version that does not match its file name. It never fails.
func (s *State) sanitize(dir string) {
	fix := func(sl *Slot) *Slot {
		if sl == nil {
			return nil
		}
		if sl.File == "" && sl.Version != "" {
			sl.File = BinaryName(dir, sl.Version)
		}
		if sl.File == "" {
			return nil
		}
		// A file outside dir would make Boot delete or exec something else.
		if filepath.Dir(filepath.Clean(sl.File)) != filepath.Clean(dir) {
			sl.File = BinaryName(dir, filepath.Base(sl.File))
		}
		if sl.Version == "" {
			sl.Version = strings.TrimPrefix(filepath.Base(sl.File), binaryPrefix)
		}
		if !validKey(strings.ReplaceAll(filepath.Base(sl.File), ".", "_")) {
			// Not a name we wrote: refuse it rather than exec it.
			return nil
		}
		return sl
	}
	s.Active = fix(s.Active)
	s.Previous = fix(s.Previous)
	s.Pending = fix(s.Pending)
	if s.Tries < 0 {
		s.Tries = 0
	}
}

// SaveState writes state.json atomically: temp file in the same directory,
// fsync, rename, fsync the directory (D9). A power cut leaves either the old
// or the new file, never a mixture.
func SaveState(dir string, st *State) error {
	if st == nil {
		return errors.New("update: save nil state")
	}
	if st.Schema == 0 {
		st.Schema = 1
	}
	if st.Schema != 1 {
		return fmt.Errorf("update: cannot write state schema %d", st.Schema)
	}
	if err := os.MkdirAll(dir, 0o755); err != nil {
		return fmt.Errorf("update: create %s: %w", dir, err)
	}
	data, err := json.MarshalIndent(st, "", "  ")
	if err != nil {
		return fmt.Errorf("update: encode state: %w", err)
	}
	tmp, err := os.CreateTemp(dir, ".state-*.json")
	if err != nil {
		return fmt.Errorf("update: create temp state: %w", err)
	}
	defer os.Remove(tmp.Name()) // no-op after a successful rename
	if err := tmp.Chmod(0o600); err != nil {
		tmp.Close()
		return fmt.Errorf("update: chmod temp state: %w", err)
	}
	if _, err := tmp.Write(append(data, '\n')); err != nil {
		tmp.Close()
		return fmt.Errorf("update: write temp state: %w", err)
	}
	if err := tmp.Sync(); err != nil {
		tmp.Close()
		return fmt.Errorf("update: fsync temp state: %w", err)
	}
	if err := tmp.Close(); err != nil {
		return fmt.Errorf("update: close temp state: %w", err)
	}
	if err := os.Rename(tmp.Name(), statePath(dir)); err != nil {
		return fmt.Errorf("update: rename state: %w", err)
	}
	syncDir(dir)
	return nil
}

// syncDir fsyncs a directory so a rename inside it survives a power cut.
// Best effort: not every filesystem supports it.
func syncDir(dir string) {
	if d, err := os.Open(dir); err == nil {
		_ = d.Sync()
		d.Close()
	}
}

// Self describes the process that is running Boot: the version string
// (git describe) and the executable path, and whether it is the factory
// binary from the read-only root.
type Self struct {
	Version string
	Path    string
	Factory bool
}

// Action is what the caller should exec. Path == "" means keep running (or,
// in the bootstrap, exec the factory binary).
type Action struct {
	Path string
	// Note says what Boot decided, for the log: "promote", "rollback",
	// "factory", "adopt", "tries" … It is not a state, only a hint.
	Note string
}

// Boot runs the state machine at start-up and returns what to exec. It is
// written to never make the process fail: every problem with the files on
// disk degrades to "keep running what we have", and the returned error is for
// the log. dir is the update directory (/data/bin); it is created when
// missing.
//
// Order:
//
//  1. A newer factory binary wins over an older active one (card reflashed,
//     /data kept): previous = active, active = nil, nothing to exec, the
//     factory binary runs.
//  2. A binary that runs from /data while the state names no active slot (a
//     wiped state.json) adopts itself as active and verified.
//  3. A pending file whose hash matches is promoted and execs; a pending file
//     that does not match is deleted.
//  4. The active file: rolled back when its hash does not match or when an
//     unverified active has used up its MaxTries starts; otherwise it is
//     counted (while unverified) and execs.
//  5. Nothing runnable: no action, the factory binary runs.
//
// Before each of those returns, unreferenced wa-transcriber-* files and
// *.part leftovers are pruned: at most three binaries stay.
// The early returns before a save (a load error, a failed save) leave the
// files alone on purpose, so the next boot starts from the same state.
func Boot(dir string, self Self, now time.Time) (Action, error) {
	if dir == "" {
		return Action{}, nil
	}
	if err := os.MkdirAll(dir, 0o755); err != nil {
		return Action{}, fmt.Errorf("update: create %s: %w", dir, err)
	}
	st, loadErr := LoadState(dir)
	if st == nil {
		st = &State{Schema: 1}
	}

	// 2. A reflash can bring a newer version than the one on /data.
	// Without a known factory version there is nothing to compare: an empty
	// version counts as "dev" in Compare, which must not win here.
	if self.Factory && self.Version != "" && st.Active != nil && Compare(self.Version, st.Active.Version) > 0 {
		st.Previous = st.Active
		st.Active = nil
		st.Pending = nil
		st.Tries = 0
		st.Verified = false
		if err := SaveState(dir, st); err != nil {
			// SaveState renames atomically: on error the old file is intact,
			// so the next boot sees the same slots again.
			return Action{}, join(loadErr, err)
		}
		prune(dir, st)
		return Action{}, join(loadErr, nil)
	}

	// A device that runs from /data but has no state (a wiped state file, a
	// fresh copy) adopts the running binary: it is proof that it starts. It is
	// not marked verified: Healthy does that after 60 s, so a binary that
	// starts and then dies is still counted by the next factory start.
	adopted := false
	if !self.Factory && st.Active == nil && self.Path != "" {
		st.Active = &Slot{Version: self.Version, File: self.Path}
		if s, err := fileSHA(self.Path); err == nil {
			st.Active.SHA256 = s
		}
		st.Tries = 0
		st.Verified = false
		adopted = true
	}
	if self.Factory && st.Active == nil && st.Pending == nil {
		// Bottom of a rollback or a fresh card: the factory binary is the
		// device, nothing to exec.
		_ = SaveState(dir, st)
		prune(dir, st)
		return Action{}, join(loadErr, nil)
	}

	// 3. Promote a pending version that matches its hash.
	if st.Pending != nil {
		if slotMatches(st.Pending) {
			oldActive, oldPrevious, oldPending := st.Active, st.Previous, st.Pending
			st.Previous = oldActive
			st.Active = oldPending
			st.Pending = nil
			st.Tries = 0
			st.Verified = false
			if err := SaveState(dir, st); err != nil {
				// The old state is still on disk: keep the pending file where
				// it is so the next boot can promote it again.
				st.Active, st.Previous, st.Pending = oldActive, oldPrevious, oldPending
				return Action{}, join(loadErr, err)
			}
			prune(dir, st) // drops `old`, which is out of the window
			// The new version may migrate session.db in place and an older
			// binary may then refuse the schema, so keep a copy of the state
			// as it is now: a rollback restores exactly this.
			dataDir := dataDirOf(dir)
			note := "promote to " + st.Active.Version
			if _, err := Snapshot(dataDir, st.Active.Version); err != nil {
				// Not fatal: the try gate still protects a crash loop, only a
				// downgrade after a schema migration loses its safety net.
				note += " (snapshot failed: " + err.Error() + ")"
			}
			// Never exec the file we are already running (see step 4).
			if samePath(self.Path, st.Active.File) {
				return Action{Note: note + " (already running)"}, join(loadErr, nil)
			}
			return Action{Path: st.Active.File, Note: note}, join(loadErr, nil)
		}
		// A pending file that does not match is worth nothing: drop it.
		_ = os.Remove(st.Pending.File)
		st.Pending = nil
	}

	// 4. The active version.
	if st.Active != nil {
		if !slotMatches(st.Active) {
			return rollback(dir, st, st.Active, "binary is missing or its sha256 does not match", now, loadErr)
		}
		// Already running the active file: the bootstrap exec'd it, so there
		// is nothing to exec and no try to count. Counting here would exec
		// the same file again on every start and reach MaxTries in a loop.
		// Only the process that is about to exec (the factory binary) counts.
		if samePath(self.Path, st.Active.File) {
			note := "running " + st.Active.Version
			if adopted {
				note = "adopt " + st.Active.Version
				if err := SaveState(dir, st); err != nil {
					return Action{}, join(loadErr, err)
				}
			}
			prune(dir, st)
			return Action{Note: note}, join(loadErr, nil)
		}
		if !st.Verified && st.Tries >= MaxTries {
			return rollback(dir, st, st.Active, fmt.Sprintf("%d starts without becoming healthy", st.Tries), now, loadErr)
		}
		note := ""
		if !st.Verified {
			st.Tries++
			note = "try"
		}
		if err := SaveState(dir, st); err != nil {
			return Action{}, join(loadErr, err)
		}
		prune(dir, st)
		return Action{Path: st.Active.File, Note: note}, join(loadErr, nil)
	}

	// Adopted slot that vanished again: nothing to do.
	if adopted {
		return Action{}, join(loadErr, nil)
	}
	_ = SaveState(dir, st)
	prune(dir, st)
	return Action{}, join(loadErr, nil)
}

// rollback moves Active back to Previous, forgets pending, records the
// failure and returns the action for the previous version (or none).
func rollback(dir string, st *State, broken *Slot, reason string, now time.Time, loadErr error) (Action, error) {
	// The broken version may already have migrated session.db: put back what
	// the version we are going back to wrote before its first start.
	if err := Restore(dataDirOf(dir), broken.Version); err != nil {
		reason += "; restoring the state snapshot failed: " + err.Error()
	}
	st.Active = st.Previous
	st.Previous = nil
	st.Pending = nil
	st.Tries = 0
	st.Verified = true
	st.LastFailure = &Failure{Version: broken.Version, Reason: reason, At: now}
	if err := SaveState(dir, st); err != nil {
		return Action{}, join(loadErr, err)
	}
	if broken.File != "" {
		_ = os.Remove(broken.File)
	}
	prune(dir, st)
	act := Action{Note: "rollback from " + broken.Version}
	if st.Active != nil {
		act.Path = st.Active.File
	}
	return act, join(loadErr, nil)
}

// Rollback swaps active and previous for the next start (manual rollback
// from the UI), drops anything pending, clears the try counter and marks the
// result verified so it is not counted again. The returned slot is the one
// that will run; nil means it is the factory binary. The caller is expected
// to Restore the matching snapshot and restart.
func Rollback(dir string, now time.Time) (*Slot, error) {
	st, err := LoadState(dir)
	if err != nil && st == nil {
		return nil, err
	}
	if st.Previous == nil {
		return nil, errors.New("update: no previous version to roll back to")
	}
	if !slotMatches(st.Previous) {
		bad := st.Previous
		st.Previous = nil
		_ = SaveState(dir, st)
		if bad.File != "" {
			_ = os.Remove(bad.File)
		}
		return nil, fmt.Errorf("update: previous binary %s is missing or damaged", bad.Version)
	}
	if st.Pending != nil {
		_ = os.Remove(st.Pending.File)
		st.Pending = nil
	}
	going := st.Active
	st.Active = st.Previous
	st.Previous = going
	st.Tries = 0
	st.Verified = true
	if going != nil {
		reason := "manual rollback"
		if err := Restore(dataDirOf(dir), going.Version); err != nil {
			reason += "; restoring the state snapshot failed: " + err.Error()
		}
		st.LastFailure = &Failure{Version: going.Version, Reason: reason, At: now}
	}
	if err := SaveState(dir, st); err != nil {
		return nil, err
	}
	prune(dir, st)
	return st.Active, join(err, nil)
}

// prune deletes every wa-transcriber-* file in dir that no slot names, the
// temp/state leftovers, and every *.part in <dir>/../updates. At most three
// binaries stay. Errors are ignored: the next boot tries again.
func prune(dir string, st *State) {
	keep := map[string]bool{}
	for _, sl := range []*Slot{st.Active, st.Previous, st.Pending} {
		if sl != nil && sl.File != "" {
			keep[filepath.Clean(sl.File)] = true
		}
	}
	entries, err := os.ReadDir(dir)
	if err == nil {
		for _, e := range entries {
			name := e.Name()
			p := filepath.Join(dir, name)
			if strings.HasPrefix(name, binaryPrefix) && !keep[filepath.Clean(p)] {
				_ = os.Remove(p)
			}
		}
	}
	// <dataDir>/updates holds the in-flight downloads; <dir> is <dataDir>/bin.
	upd := filepath.Join(filepath.Dir(filepath.Clean(dir)), "updates")
	if parts, err := filepath.Glob(filepath.Join(upd, "*.part")); err == nil {
		for _, p := range parts {
			_ = os.Remove(p)
		}
	}
}

// join combines a boot error with a secondary one without losing either.
func join(a, b error) error { return errors.Join(a, b) }

// dataDirOf is the data directory that holds dir: /data for /data/bin. The
// package derives it rather than taking a second path, because every caller
// already builds dir as <dataDir>/bin (see Installer.updateDir).
func dataDirOf(dir string) string { return filepath.Dir(filepath.Clean(dir)) }

// samePath reports whether two paths name the same file, following symlinks.
// It answers "is this process already running that file?", which is what keeps
// Boot from exec'ing itself forever.
func samePath(a, b string) bool {
	if a == "" || b == "" {
		return false
	}
	if filepath.Clean(a) == filepath.Clean(b) {
		return true
	}
	fa, err := os.Stat(a)
	if err != nil {
		return false
	}
	fb, err := os.Stat(b)
	if err != nil {
		return false
	}
	return os.SameFile(fa, fb)
}

// Healthy marks the running version as started successfully: the try counter
// is cleared and the active slot stops being counted on the next boot. main
// calls it on a timer after 60 s of uptime, never at start, so a process that
// dies immediately is still counted by the next factory start.
func Healthy(dir string) error {
	if dir == "" {
		return nil
	}
	st, err := LoadState(dir)
	if err != nil && st == nil {
		return err
	}
	if st.Active == nil || (st.Verified && st.Tries == 0) {
		return join(err, nil)
	}
	st.Verified = true
	st.Tries = 0
	return join(err, SaveState(dir, st))
}
