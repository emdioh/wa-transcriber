package update

import (
	"encoding/json"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

// binDirs returns a data directory with the bin and updates subdirectories.
func binDirs(t *testing.T) (dataDir, binDir, updDir string) {
	t.Helper()
	dataDir = t.TempDir()
	binDir = filepath.Join(dataDir, "bin")
	updDir = filepath.Join(dataDir, "updates")
	for _, d := range []string{binDir, updDir} {
		if err := os.MkdirAll(d, 0o755); err != nil {
			t.Fatal(err)
		}
	}
	return dataDir, binDir, updDir
}

// fakeBinary writes a file with the given content and mode and returns its
// path and sha256. The content does not have to be a real ELF: Boot only
// hashes and execs (the exec is the caller's), it does not parse.
func fakeBinary(t *testing.T, dir, version, content string, mode os.FileMode) (string, string) {
	t.Helper()
	p := BinaryName(dir, version)
	if err := os.WriteFile(p, []byte(content), mode); err != nil {
		t.Fatal(err)
	}
	if err := os.Chmod(p, mode); err != nil {
		t.Fatal(err)
	}
	return p, Sum([]byte(content))
}

func mustSave(t *testing.T, dir string, st *State) {
	t.Helper()
	if err := SaveState(dir, st); err != nil {
		t.Fatal(err)
	}
}

func mustLoad(t *testing.T, dir string) *State {
	t.Helper()
	st, err := LoadState(dir)
	if err != nil {
		t.Fatalf("LoadState: %v", err)
	}
	return st
}

func exists(p string) bool {
	_, err := os.Stat(p)
	return err == nil
}

// factoryV is the version of the factory binary in the tests that do not test
// the reflash case: old enough to lose against every test slot.
const factoryV = "v0.0.1"

var testNow = time.Date(2026, 11, 22, 12, 0, 0, 0, time.UTC)

func TestLoadState(t *testing.T) {
	t.Run("missing file", func(t *testing.T) {
		dir := t.TempDir()
		st, err := LoadState(dir)
		if err != nil {
			t.Fatalf("LoadState: %v", err)
		}
		if st == nil || st.Schema != 1 || st.Active != nil || st.Pending != nil {
			t.Fatalf("state = %+v, want empty schema 1", st)
		}
	})
	t.Run("round trip", func(t *testing.T) {
		dir := t.TempDir()
		want := &State{
			Schema:      1,
			Active:      &Slot{Version: "v1", File: filepath.Join(dir, "wa-transcriber-v1"), SHA256: goodSHA},
			Previous:    &Slot{Version: "v0", File: filepath.Join(dir, "wa-transcriber-v0"), SHA256: goodSHA},
			Tries:       2,
			Verified:    true,
			LastFailure: &Failure{Version: "v0.9", Reason: "test", At: testNow},
		}
		mustSave(t, dir, want)
		// 0600, like config.json.
		fi, err := os.Stat(statePath(dir))
		if err != nil {
			t.Fatal(err)
		}
		if perm := fi.Mode().Perm(); perm != 0o600 {
			t.Errorf("state.json mode = %o, want 600", perm)
		}
		got := mustLoad(t, dir)
		if got.Active == nil || got.Active.Version != "v1" || got.Tries != 2 || !got.Verified ||
			got.Previous == nil || got.LastFailure == nil || got.LastFailure.Reason != "test" {
			t.Fatalf("round trip lost data: %+v", got)
		}
		if got.Pending != nil {
			t.Error("pending came back from nowhere")
		}
	})
	t.Run("corrupt json is moved aside", func(t *testing.T) {
		dir := t.TempDir()
		if err := os.WriteFile(statePath(dir), []byte(`{"schema":1,"active":`), 0o600); err != nil {
			t.Fatal(err)
		}
		st, err := LoadState(dir)
		if err != nil {
			t.Fatalf("LoadState must not fail on bad content: %v", err)
		}
		if st.Schema != 1 || st.Active != nil {
			t.Fatalf("state = %+v, want empty", st)
		}
		if exists(statePath(dir)) {
			t.Error("bad state.json was not moved away")
		}
		if !exists(statePath(dir) + ".bad") {
			t.Error("state.json.bad missing")
		}
	})
	t.Run("empty file is moved aside", func(t *testing.T) {
		dir := t.TempDir()
		if err := os.WriteFile(statePath(dir), nil, 0o600); err != nil {
			t.Fatal(err)
		}
		st, err := LoadState(dir)
		if err != nil || st.Active != nil {
			t.Fatalf("LoadState = %+v, %v", st, err)
		}
		if !exists(statePath(dir) + ".bad") {
			t.Error("state.json.bad missing")
		}
	})
	t.Run("future schema is moved aside", func(t *testing.T) {
		dir := t.TempDir()
		if err := os.WriteFile(statePath(dir), []byte(`{"schema":2,"active":{"version":"v9","file":"/x"}}`), 0o600); err != nil {
			t.Fatal(err)
		}
		st, err := LoadState(dir)
		if err != nil || st.Active != nil {
			t.Fatalf("LoadState = %+v, %v; schema 2 must not be adopted", st, err)
		}
	})
	t.Run("foreign paths are clamped into dir", func(t *testing.T) {
		dir := t.TempDir()
		raw := `{"schema":1,"active":{"version":"v1","file":"/etc/passwd"}}`
		if err := os.WriteFile(statePath(dir), []byte(raw), 0o600); err != nil {
			t.Fatal(err)
		}
		st, err := LoadState(dir)
		if err != nil {
			t.Fatal(err)
		}
		if st.Active == nil || filepath.Dir(st.Active.File) != dir {
			t.Fatalf("active = %+v, want a file inside %s", st.Active, dir)
		}
	})
}

func TestSaveStateNoNil(t *testing.T) {
	if err := SaveState(t.TempDir(), nil); err == nil {
		t.Fatal("SaveState(nil) must fail")
	}
}

func TestBoot(t *testing.T) {
	t.Run("no state, factory", func(t *testing.T) {
		_, bin, _ := binDirs(t)
		act, err := Boot(bin, Self{Version: "v0.1.0", Factory: true}, testNow)
		if err != nil {
			t.Fatalf("Boot: %v", err)
		}
		if act.Path != "" {
			t.Fatalf("action = %+v, want none (factory runs)", act)
		}
		st := mustLoad(t, bin)
		if st.Active != nil || st.Pending != nil {
			t.Fatalf("state = %+v", st)
		}
	})
	t.Run("promote pending", func(t *testing.T) {
		_, bin, _ := binDirs(t)
		oldFile, oldSHA := fakeBinary(t, bin, "v0.1.0", "old", 0o755)
		newFile, newSHA := fakeBinary(t, bin, "v0.2.0", "new", 0o755)
		mustSave(t, bin, &State{
			Schema:  1,
			Active:  &Slot{Version: "v0.1.0", File: oldFile, SHA256: oldSHA},
			Pending: &Slot{Version: "v0.2.0", File: newFile, SHA256: newSHA},
			Tries:   MaxTries, Verified: true,
		})
		act, err := Boot(bin, Self{Version: "v0.1.0", Path: oldFile, Factory: true}, testNow)
		if err != nil {
			t.Fatalf("Boot: %v", err)
		}
		if act.Path != newFile || !strings.Contains(act.Note, "promote") {
			t.Fatalf("action = %+v, want promote to %s", act, newFile)
		}
		st := mustLoad(t, bin)
		if st.Active == nil || st.Active.Version != "v0.2.0" || st.Pending != nil {
			t.Fatalf("state = %+v", st)
		}
		if st.Tries != 0 || st.Verified {
			t.Fatalf("promote must restart the health gate: tries=%d verified=%v", st.Tries, st.Verified)
		}
		if st.Previous == nil || st.Previous.Version != "v0.1.0" {
			t.Fatalf("previous = %+v", st.Previous)
		}
		// The slot that fell out of the window was the previous of the old
		// active: none here, so the old active is now previous and lives on.
		if !exists(oldFile) || !exists(newFile) {
			t.Fatal("a binary that is still a slot was deleted")
		}
	})
	t.Run("promote drops the old previous file", func(t *testing.T) {
		_, bin, _ := binDirs(t)
		prevFile, prevSHA := fakeBinary(t, bin, "v0.0.9", "prev", 0o755)
		oldFile, oldSHA := fakeBinary(t, bin, "v0.1.0", "old", 0o755)
		newFile, newSHA := fakeBinary(t, bin, "v0.2.0", "new", 0o755)
		mustSave(t, bin, &State{
			Schema:   1,
			Active:   &Slot{Version: "v0.1.0", File: oldFile, SHA256: oldSHA},
			Previous: &Slot{Version: "v0.0.9", File: prevFile, SHA256: prevSHA},
			Pending:  &Slot{Version: "v0.2.0", File: newFile, SHA256: newSHA},
		})
		if _, err := Boot(bin, Self{Version: factoryV, Factory: true}, testNow); err != nil {
			t.Fatal(err)
		}
		if exists(prevFile) {
			t.Fatal("the previous version that fell out of the window was not deleted")
		}
		if !exists(oldFile) || !exists(newFile) {
			t.Fatal("active/pending binaries must survive the promote")
		}
	})
	t.Run("pending with wrong hash is dropped", func(t *testing.T) {
		_, bin, _ := binDirs(t)
		oldFile, oldSHA := fakeBinary(t, bin, "v0.1.0", "old", 0o755)
		newFile, _ := fakeBinary(t, bin, "v0.2.0", "new", 0o755)
		mustSave(t, bin, &State{
			Schema:  1,
			Active:  &Slot{Version: "v0.1.0", File: oldFile, SHA256: oldSHA},
			Pending: &Slot{Version: "v0.2.0", File: newFile, SHA256: goodSHA},
		})
		act, err := Boot(bin, Self{Version: factoryV, Factory: true}, testNow)
		if err != nil {
			t.Fatal(err)
		}
		if act.Path != oldFile {
			t.Fatalf("action = %+v, want the old active", act)
		}
		if exists(newFile) {
			t.Fatal("a pending binary with the wrong hash was kept")
		}
		if st := mustLoad(t, bin); st.Pending != nil || st.Active.Version != "v0.1.0" {
			t.Fatalf("state = %+v", st)
		}
	})
	t.Run("rollback after MaxTries", func(t *testing.T) {
		_, bin, _ := binDirs(t)
		prevFile, prevSHA := fakeBinary(t, bin, "v0.1.0", "prev", 0o755)
		badFile, badSHA := fakeBinary(t, bin, "v0.2.0", "bad", 0o755)
		mustSave(t, bin, &State{
			Schema:   1,
			Active:   &Slot{Version: "v0.2.0", File: badFile, SHA256: badSHA},
			Previous: &Slot{Version: "v0.1.0", File: prevFile, SHA256: prevSHA},
			Tries:    MaxTries,
		})
		act, err := Boot(bin, Self{Version: factoryV, Factory: true}, testNow)
		if err != nil {
			t.Fatal(err)
		}
		if act.Path != prevFile || !strings.Contains(act.Note, "rollback") {
			t.Fatalf("action = %+v, want rollback to %s", act, prevFile)
		}
		st := mustLoad(t, bin)
		if st.Active == nil || st.Active.Version != "v0.1.0" || st.Previous != nil {
			t.Fatalf("state = %+v", st)
		}
		if st.Tries != 0 || !st.Verified {
			t.Fatalf("rollback must mark the result verified: %+v", st)
		}
		if st.LastFailure == nil || st.LastFailure.Version != "v0.2.0" || st.LastFailure.At != testNow {
			t.Fatalf("last failure = %+v", st.LastFailure)
		}
		if exists(badFile) {
			t.Fatal("the broken binary was not deleted")
		}
	})
	t.Run("tries counter stops at MaxTries", func(t *testing.T) {
		_, bin, _ := binDirs(t)
		prevFile, prevSHA := fakeBinary(t, bin, "v0.1.0", "prev", 0o755)
		badFile, badSHA := fakeBinary(t, bin, "v0.2.0", "bad", 0o755)
		mustSave(t, bin, &State{
			Schema:   1,
			Active:   &Slot{Version: "v0.2.0", File: badFile, SHA256: badSHA},
			Previous: &Slot{Version: "v0.1.0", File: prevFile, SHA256: prevSHA},
		})
		for i := 1; i <= MaxTries; i++ {
			act, err := Boot(bin, Self{Version: factoryV, Factory: true}, testNow)
			if err != nil {
				t.Fatal(err)
			}
			if act.Path != badFile {
				t.Fatalf("try %d: action = %+v, want the unverified active", i, act)
			}
			if st := mustLoad(t, bin); st.Tries != i {
				t.Fatalf("try %d: tries = %d", i, st.Tries)
			}
		}
		// The next boot rolls back.
		act, err := Boot(bin, Self{Version: factoryV, Factory: true}, testNow)
		if err != nil {
			t.Fatal(err)
		}
		if act.Path != prevFile {
			t.Fatalf("action = %+v, want rollback after %d tries", act, MaxTries)
		}
	})
	t.Run("verified active is never counted", func(t *testing.T) {
		_, bin, _ := binDirs(t)
		activeFile, activeSHA := fakeBinary(t, bin, "v0.1.0", "active", 0o755)
		mustSave(t, bin, &State{
			Schema:   1,
			Active:   &Slot{Version: "v0.1.0", File: activeFile, SHA256: activeSHA},
			Tries:    7,
			Verified: true,
		})
		act, err := Boot(bin, Self{Version: factoryV, Factory: true}, testNow)
		if err != nil {
			t.Fatal(err)
		}
		if act.Path != activeFile {
			t.Fatalf("action = %+v", act)
		}
		if st := mustLoad(t, bin); st.Tries != 7 || st.Active.Version != "v0.1.0" {
			t.Fatalf("state = %+v", st)
		}
	})
	t.Run("active with wrong hash rolls back", func(t *testing.T) {
		_, bin, _ := binDirs(t)
		prevFile, prevSHA := fakeBinary(t, bin, "v0.1.0", "prev", 0o755)
		badFile, _ := fakeBinary(t, bin, "v0.2.0", "bad", 0o755)
		mustSave(t, bin, &State{
			Schema:   1,
			Active:   &Slot{Version: "v0.2.0", File: badFile, SHA256: goodSHA},
			Previous: &Slot{Version: "v0.1.0", File: prevFile, SHA256: prevSHA},
			Verified: true,
		})
		act, err := Boot(bin, Self{Version: factoryV, Factory: true}, testNow)
		if err != nil {
			t.Fatal(err)
		}
		if act.Path != prevFile {
			t.Fatalf("action = %+v, want rollback", act)
		}
		if !strings.Contains(mustLoad(t, bin).LastFailure.Reason, "sha256") {
			t.Fatalf("reason = %q", mustLoad(t, bin).LastFailure.Reason)
		}
	})
	t.Run("active file missing rolls back", func(t *testing.T) {
		_, bin, _ := binDirs(t)
		prevFile, prevSHA := fakeBinary(t, bin, "v0.1.0", "prev", 0o755)
		mustSave(t, bin, &State{
			Schema:   1,
			Active:   &Slot{Version: "v0.2.0", File: BinaryName(bin, "v0.2.0"), SHA256: goodSHA},
			Previous: &Slot{Version: "v0.1.0", File: prevFile, SHA256: prevSHA},
		})
		act, err := Boot(bin, Self{Version: factoryV, Factory: true}, testNow)
		if err != nil {
			t.Fatal(err)
		}
		if act.Path != prevFile {
			t.Fatalf("action = %+v, want rollback", act)
		}
	})
	t.Run("rollback with no previous runs the factory", func(t *testing.T) {
		_, bin, _ := binDirs(t)
		badFile, badSHA := fakeBinary(t, bin, "v0.2.0", "bad", 0o755)
		mustSave(t, bin, &State{
			Schema: 1,
			Active: &Slot{Version: "v0.2.0", File: badFile, SHA256: badSHA},
			Tries:  MaxTries,
		})
		act, err := Boot(bin, Self{Version: factoryV, Factory: true}, testNow)
		if err != nil {
			t.Fatal(err)
		}
		if act.Path != "" {
			t.Fatalf("action = %+v, want none (factory)", act)
		}
		st := mustLoad(t, bin)
		if st.Active != nil || st.LastFailure == nil {
			t.Fatalf("state = %+v", st)
		}
	})
	t.Run("newer factory wins", func(t *testing.T) {
		_, bin, _ := binDirs(t)
		oldFile, oldSHA := fakeBinary(t, bin, "v0.1.0", "old", 0o755)
		newFile, newSHA := fakeBinary(t, bin, "v0.2.0", "new", 0o755)
		mustSave(t, bin, &State{
			Schema:   1,
			Active:   &Slot{Version: "v0.1.0", File: oldFile, SHA256: oldSHA},
			Pending:  &Slot{Version: "v0.2.0", File: newFile, SHA256: newSHA},
			Tries:    2,
			Verified: true,
		})
		act, err := Boot(bin, Self{Version: "v0.3.0", Factory: true}, testNow)
		if err != nil {
			t.Fatal(err)
		}
		if act.Path != "" {
			t.Fatalf("action = %+v, want none: the factory binary runs", act)
		}
		st := mustLoad(t, bin)
		if st.Active != nil || st.Pending != nil || st.Tries != 0 || st.Verified {
			t.Fatalf("state = %+v", st)
		}
		if st.Previous == nil || st.Previous.Version != "v0.1.0" {
			t.Fatalf("previous = %+v, want the old active", st.Previous)
		}
		if exists(newFile) {
			t.Fatal("the pending binary that fell out of the window was kept")
		}
		if !exists(oldFile) {
			t.Fatal("the old active is now previous and must stay")
		}
	})
	t.Run("factory equal or older does not win", func(t *testing.T) {
		_, bin, _ := binDirs(t)
		activeFile, activeSHA := fakeBinary(t, bin, "v0.1.0", "active", 0o755)
		for _, v := range []string{"v0.1.0", "v0.0.9"} {
			mustSave(t, bin, &State{
				Schema:   1,
				Active:   &Slot{Version: "v0.1.0", File: activeFile, SHA256: activeSHA},
				Verified: true,
			})
			act, err := Boot(bin, Self{Version: v, Factory: true}, testNow)
			if err != nil {
				t.Fatal(err)
			}
			if act.Path != activeFile {
				t.Fatalf("factory %s: action = %+v, want the newer active", v, act)
			}
		}
	})
	t.Run("lost state from /data adopts itself", func(t *testing.T) {
		_, bin, _ := binDirs(t)
		selfPath, _ := fakeBinary(t, bin, "v0.2.0", "self", 0o755)
		act, err := Boot(bin, Self{Version: "v0.2.0", Path: selfPath, Factory: false}, testNow)
		if err != nil {
			t.Fatal(err)
		}
		// The running binary must not exec itself: only the factory binary
		// counts tries and execs, or the same file would exec forever.
		if act.Path != "" {
			t.Fatalf("action = %+v, want no exec (already running)", act)
		}
		st := mustLoad(t, bin)
		if st.Active == nil || st.Active.File != selfPath || st.Verified || st.Tries != 0 {
			t.Fatalf("state = %+v, want the adopted, unverified active", st)
		}
		if st.Active.SHA256 == "" {
			t.Fatal("adopted slot has no hash")
		}
		if err := Healthy(bin); err != nil {
			t.Fatal(err)
		}
		if st := mustLoad(t, bin); !st.Verified || st.Tries != 0 {
			t.Fatalf("state after Healthy = %+v, want verified", st)
		}
	})
	t.Run("running the active slot never execs itself", func(t *testing.T) {
		_, bin, _ := binDirs(t)
		selfPath, selfSHA := fakeBinary(t, bin, "v0.2.0", "active", 0o755)
		for _, verified := range []bool{true, false} {
			mustSave(t, bin, &State{
				Schema:   1,
				Active:   &Slot{Version: "v0.2.0", File: selfPath, SHA256: selfSHA},
				Verified: verified,
			})
			act, err := Boot(bin, Self{Version: "v0.2.0", Path: selfPath}, testNow)
			if err != nil {
				t.Fatal(err)
			}
			if act.Path != "" {
				t.Fatalf("verified=%v: action = %+v, want no exec", verified, act)
			}
			if st := mustLoad(t, bin); st.Tries != 0 {
				t.Fatalf("verified=%v: tries = %d, a running binary must not be counted", verified, st.Tries)
			}
		}
	})
	t.Run("a /data binary execs a newer pending one", func(t *testing.T) { // The bootstrap path: an active binary stages a pending version, then
		// execs the factory binary, which promotes and execs the new file.
		_, bin, _ := binDirs(t)
		selfPath, selfSHA := fakeBinary(t, bin, "v0.2.0", "active", 0o755)
		newPath, newSHA := fakeBinary(t, bin, "v0.3.0", "new", 0o755)
		mustSave(t, bin, &State{
			Schema:   1,
			Active:   &Slot{Version: "v0.2.0", File: selfPath, SHA256: selfSHA},
			Pending:  &Slot{Version: "v0.3.0", File: newPath, SHA256: newSHA},
			Verified: true,
		})
		act, err := Boot(bin, Self{Version: factoryV, Factory: true}, testNow)
		if err != nil {
			t.Fatal(err)
		}
		if act.Path != newPath {
			t.Fatalf("action = %+v, want the pending binary", act)
		}
		st := mustLoad(t, bin)
		if st.Active.Version != "v0.3.0" || st.Previous == nil || st.Previous.Version != "v0.2.0" {
			t.Fatalf("state = %+v, want active v0.3.0 over previous v0.2.0", st)
		}
		if st.Verified {
			t.Fatal("a promoted version starts unverified")
		}
	})
	t.Run("lost state from /data keeps its previous slot", func(t *testing.T) {
		_, bin, _ := binDirs(t)
		prevFile, prevSHA := fakeBinary(t, bin, "v0.1.0", "prev", 0o755)
		selfPath, _ := fakeBinary(t, bin, "v0.2.0", "self", 0o755)
		mustSave(t, bin, &State{Schema: 1, Previous: &Slot{Version: "v0.1.0", File: prevFile, SHA256: prevSHA}})
		if _, err := Boot(bin, Self{Version: "v0.2.0", Path: selfPath}, testNow); err != nil {
			t.Fatal(err)
		}
		st := mustLoad(t, bin)
		if st.Previous == nil || st.Previous.Version != "v0.1.0" || st.Active.Version != "v0.2.0" {
			t.Fatalf("state = %+v", st)
		}
	})
	t.Run("stray fourth binary and .part are pruned", func(t *testing.T) {
		_, bin, upd := binDirs(t)
		activeFile, activeSHA := fakeBinary(t, bin, "v0.2.0", "active", 0o755)
		prevFile, prevSHA := fakeBinary(t, bin, "v0.1.0", "prev", 0o755)
		stray, _ := fakeBinary(t, bin, "v0.3.0", "stray", 0o755)
		part := filepath.Join(upd, "wa-transcriber-v0.4.0.part")
		if err := os.WriteFile(part, []byte("partial"), 0o755); err != nil {
			t.Fatal(err)
		}
		notOurs := filepath.Join(bin, "something-else")
		if err := os.WriteFile(notOurs, []byte("keep"), 0o644); err != nil {
			t.Fatal(err)
		}
		mustSave(t, bin, &State{
			Schema:   1,
			Active:   &Slot{Version: "v0.2.0", File: activeFile, SHA256: activeSHA},
			Previous: &Slot{Version: "v0.1.0", File: prevFile, SHA256: prevSHA},
			Verified: true,
		})
		if _, err := Boot(bin, Self{Version: factoryV, Factory: true}, testNow); err != nil {
			t.Fatal(err)
		}
		if exists(stray) {
			t.Fatal("stray fourth binary was not pruned")
		}
		if exists(part) {
			t.Fatal("stray .part was not pruned")
		}
		if !exists(activeFile) || !exists(prevFile) {
			t.Fatal("slot binaries were pruned")
		}
		if !exists(notOurs) {
			t.Fatal("prune deleted a file that is not ours")
		}
	})
	t.Run("corrupt state.json becomes empty and boots", func(t *testing.T) {
		_, bin, _ := binDirs(t)
		if err := os.WriteFile(statePath(bin), []byte("\x00\x00garbage"), 0o600); err != nil {
			t.Fatal(err)
		}
		act, err := Boot(bin, Self{Version: "v0.1.0", Factory: true}, testNow)
		if err != nil {
			t.Fatalf("Boot must not fail on a corrupt state: %v", err)
		}
		if act.Path != "" {
			t.Fatalf("action = %+v, want none", act)
		}
		if !exists(statePath(bin) + ".bad") {
			t.Fatal("corrupt state was not kept as state.json.bad")
		}
		// Boot writes a clean state again, so the next start has a real file.
		if !exists(statePath(bin)) {
			t.Fatal("Boot did not rewrite a clean state.json")
		}
		if st := mustLoad(t, bin); st.Active != nil || st.Pending != nil {
			t.Fatalf("state = %+v, want empty", st)
		}
	})
	t.Run("empty dir argument is inert", func(t *testing.T) {
		act, err := Boot("", Self{Version: "dev"}, testNow)
		if act.Path != "" || err != nil {
			t.Fatalf("Boot(\"\") = %+v, %v", act, err)
		}
	})
	t.Run("missing dir is created", func(t *testing.T) {
		data := t.TempDir()
		bin := filepath.Join(data, "bin")
		if _, err := Boot(bin, Self{Version: "v0.1.0", Factory: true}, testNow); err != nil {
			t.Fatal(err)
		}
		if fi, err := os.Stat(bin); err != nil || !fi.IsDir() {
			t.Fatalf("bin dir: %v", err)
		}
	})
	t.Run("promote snapshots the data, rollback restores it", func(t *testing.T) {
		// A new version may migrate session.db in place; going back to the
		// previous one needs the files as they were before its first start.
		data, bin, _ := binDirs(t)
		cfgPath := filepath.Join(data, "config.json")
		dbPath := filepath.Join(data, "session.db")
		if err := os.WriteFile(cfgPath, []byte("before"), 0o600); err != nil {
			t.Fatal(err)
		}
		// The board's WAL is bigger than the db: both must travel.
		if err := os.WriteFile(dbPath, []byte("db"), 0o600); err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(dbPath+"-wal", []byte("wal"), 0o600); err != nil {
			t.Fatal(err)
		}
		prevFile, prevSHA := fakeBinary(t, bin, "v0.1.0", "prev", 0o755)
		newFile, newSHA := fakeBinary(t, bin, "v0.2.0", "new", 0o755)
		mustSave(t, bin, &State{
			Schema:   1,
			Active:   &Slot{Version: "v0.1.0", File: prevFile, SHA256: prevSHA},
			Pending:  &Slot{Version: "v0.2.0", File: newFile, SHA256: newSHA},
			Verified: true,
		})
		act, err := Boot(bin, Self{Version: factoryV, Factory: true}, testNow)
		if err != nil {
			t.Fatal(err)
		}
		if act.Path != newFile {
			t.Fatalf("action = %+v, want the pending binary", act)
		}
		snap := filepath.Join(data, "rollback", "v0.2.0")
		for _, name := range []string{"config.json", "session.db", "session.db-wal"} {
			if !exists(filepath.Join(snap, name)) {
				t.Fatalf("snapshot is missing %s", name)
			}
		}
		// The new version migrates and writes; a manual rollback restores.
		if err := os.WriteFile(cfgPath, []byte("after-migration"), 0o600); err != nil {
			t.Fatal(err)
		}
		if err := os.Remove(dbPath + "-wal"); err != nil {
			t.Fatal(err)
		}
		if _, err := Rollback(bin, testNow); err != nil {
			t.Fatalf("Rollback: %v", err)
		}
		if b, err := os.ReadFile(cfgPath); err != nil || string(b) != "before" {
			t.Fatalf("config.json = %q (%v), want the snapshot", b, err)
		}
		if !exists(dbPath + "-wal") {
			t.Fatal("the WAL was not restored")
		}
	})
}

func TestBootStateFileIsValidJSON(t *testing.T) {
	_, bin, _ := binDirs(t)
	activeFile, activeSHA := fakeBinary(t, bin, "v0.1.0", "a", 0o755)
	mustSave(t, bin, &State{Schema: 1, Active: &Slot{Version: "v0.1.0", File: activeFile, SHA256: activeSHA}})
	if _, err := Boot(bin, Self{Version: factoryV, Factory: true}, testNow); err != nil {
		t.Fatal(err)
	}
	b, err := os.ReadFile(statePath(bin))
	if err != nil {
		t.Fatal(err)
	}
	var m map[string]any
	if err := json.Unmarshal(b, &m); err != nil {
		t.Fatalf("state.json is not valid JSON: %v", err)
	}
	if m["tries"].(float64) != 1 {
		t.Fatalf("tries = %v, want 1", m["tries"])
	}
}

func TestRollback(t *testing.T) {
	t.Run("swaps active and previous", func(t *testing.T) {
		_, bin, _ := binDirs(t)
		aFile, aSHA := fakeBinary(t, bin, "v0.2.0", "a", 0o755)
		bFile, bSHA := fakeBinary(t, bin, "v0.1.0", "b", 0o755)
		pendingFile, pendingSHA := fakeBinary(t, bin, "v0.3.0", "p", 0o755)
		mustSave(t, bin, &State{
			Schema:   1,
			Active:   &Slot{Version: "v0.2.0", File: aFile, SHA256: aSHA},
			Previous: &Slot{Version: "v0.1.0", File: bFile, SHA256: bSHA},
			Pending:  &Slot{Version: "v0.3.0", File: pendingFile, SHA256: pendingSHA},
			Tries:    2,
		})
		slot, err := Rollback(bin, testNow)
		if err != nil {
			t.Fatalf("Rollback: %v", err)
		}
		if slot == nil || slot.Version != "v0.1.0" {
			t.Fatalf("slot = %+v, want v0.1.0", slot)
		}
		st := mustLoad(t, bin)
		if st.Active.Version != "v0.1.0" || st.Previous.Version != "v0.2.0" {
			t.Fatalf("state = %+v", st)
		}
		if st.Pending != nil || st.Tries != 0 || !st.Verified {
			t.Fatalf("state = %+v", st)
		}
		if st.LastFailure == nil || st.LastFailure.Version != "v0.2.0" {
			t.Fatalf("last failure = %+v", st.LastFailure)
		}
		if exists(pendingFile) {
			t.Fatal("pending file survived a manual rollback")
		}
		// A second manual rollback goes back.
		if _, err := Rollback(bin, testNow); err != nil {
			t.Fatalf("second Rollback: %v", err)
		}
		if st := mustLoad(t, bin); st.Active.Version != "v0.2.0" {
			t.Fatalf("second rollback: active = %s", st.Active.Version)
		}
	})
	t.Run("nothing to roll back to", func(t *testing.T) {
		_, bin, _ := binDirs(t)
		if _, err := Rollback(bin, testNow); err == nil {
			t.Fatal("Rollback without a previous version must fail")
		}
	})
	t.Run("previous binary damaged", func(t *testing.T) {
		_, bin, _ := binDirs(t)
		aFile, aSHA := fakeBinary(t, bin, "v0.2.0", "a", 0o755)
		mustSave(t, bin, &State{
			Schema:   1,
			Active:   &Slot{Version: "v0.2.0", File: aFile, SHA256: aSHA},
			Previous: &Slot{Version: "v0.1.0", File: BinaryName(bin, "v0.1.0"), SHA256: goodSHA},
		})
		if _, err := Rollback(bin, testNow); err == nil {
			t.Fatal("Rollback to a missing binary must fail")
		}
		st := mustLoad(t, bin)
		if st.Active == nil || st.Active.Version != "v0.2.0" || st.Previous != nil {
			t.Fatalf("state = %+v", st)
		}
	})
}
