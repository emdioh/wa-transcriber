package update

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
)

// deviceState writes a fake /data with config.json, session.db and its
// sidecars, plus a stray file that must not be copied.
func deviceState(t *testing.T) string {
	t.Helper()
	data := t.TempDir()
	files := map[string][]byte{
		"config.json":    []byte(`{"apikey":"secret","language":"it"}`),
		"session.db":     []byte("SQLite format 3\x00" + strings.Repeat("d", 544<<10)),
		"session.db-wal": []byte(strings.Repeat("w", 4<<20)),
		"session.db-shm": []byte("shm-bytes"),
		"other.txt":      []byte("not ours"),
	}
	for name, body := range files {
		if err := os.WriteFile(filepath.Join(data, name), body, 0o600); err != nil {
			t.Fatal(err)
		}
	}
	return data
}

func readAll(t *testing.T, path string) []byte {
	t.Helper()
	b, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	return b
}

func TestSnapshotRestore(t *testing.T) {
	data := deviceState(t)
	want := map[string][]byte{}
	for _, name := range snapshotNames {
		want[name] = readAll(t, filepath.Join(data, name))
	}

	dir, err := Snapshot(data, "v0.1.1")
	if err != nil {
		t.Fatalf("Snapshot: %v", err)
	}
	if dir != filepath.Join(data, "rollback", "v0.1.1") {
		t.Fatalf("snapshot dir = %s", dir)
	}
	for name, body := range want {
		p := filepath.Join(dir, name)
		if got := readAll(t, p); string(got) != string(body) {
			t.Fatalf("%s: snapshot differs (%d bytes, want %d)", name, len(got), len(body))
		}
		fi, err := os.Stat(p)
		if err != nil {
			t.Fatal(err)
		}
		if perm := fi.Mode().Perm(); perm != 0o600 {
			t.Errorf("%s mode = %o, want 600", name, perm)
		}
	}
	if exists(filepath.Join(dir, "other.txt")) {
		t.Fatal("Snapshot copied a file that is not device state")
	}

	// The new version runs and changes everything, including the WAL.
	newConfig := []byte(`{"apikey":"secret","language":"en","update_dir":"/data/bin"}`)
	if err := os.WriteFile(filepath.Join(data, "config.json"), newConfig, 0o600); err != nil {
		t.Fatal(err)
	}
	for _, name := range []string{"session.db", "session.db-wal", "session.db-shm"} {
		if err := os.WriteFile(filepath.Join(data, name), []byte("migrated-"+name), 0o600); err != nil {
			t.Fatal(err)
		}
	}

	if err := Restore(data, "v0.1.1"); err != nil {
		t.Fatalf("Restore: %v", err)
	}
	for name, body := range want {
		if got := readAll(t, filepath.Join(data, name)); string(got) != string(body) {
			t.Fatalf("%s not restored (%d bytes, want %d)", name, len(got), len(body))
		}
	}

	// The snapshot itself is still intact: a rollback can be repeated.
	if err := Restore(data, "v0.1.1"); err != nil {
		t.Fatalf("second Restore: %v", err)
	}
}

func TestSnapshotMissingFiles(t *testing.T) {
	data := t.TempDir()
	// Only config.json: no WhatsApp session yet.
	if err := os.WriteFile(filepath.Join(data, "config.json"), []byte(`{}`), 0o600); err != nil {
		t.Fatal(err)
	}
	dir, err := Snapshot(data, "v0.2.0")
	if err != nil {
		t.Fatalf("Snapshot: %v", err)
	}
	if !exists(filepath.Join(dir, "config.json")) {
		t.Fatal("config.json not snapshotted")
	}
	if exists(filepath.Join(dir, "session.db")) {
		t.Fatal("a session.db appeared out of nowhere")
	}
	// And an empty data dir is not an error either.
	if _, err := Snapshot(t.TempDir(), "v0.2.0"); err != nil {
		t.Fatalf("Snapshot of an empty dir: %v", err)
	}
}

func TestRestoreDropsStaleSidecars(t *testing.T) {
	data := deviceState(t)
	// Take a snapshot with a checkpointed database (no -wal, no -shm).
	if _, err := Snapshot(data, "v0.1.0"); err != nil {
		t.Fatal(err)
	}
	snap := filepath.Join(data, "rollback", "v0.1.0")
	for _, name := range []string{"session.db-wal", "session.db-shm"} {
		if err := os.Remove(filepath.Join(snap, name)); err != nil {
			t.Fatal(err)
		}
	}
	// The newer version left a big WAL next to the migrated database.
	if err := os.WriteFile(filepath.Join(data, "session.db"), []byte("migrated"), 0o600); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(data, "session.db-wal"), []byte("stale wal"), 0o600); err != nil {
		t.Fatal(err)
	}
	if err := Restore(data, "v0.1.0"); err != nil {
		t.Fatalf("Restore: %v", err)
	}
	if exists(filepath.Join(data, "session.db-wal")) {
		t.Fatal("a stale WAL of the newer version survived the restore")
	}
	if got := string(readAll(t, filepath.Join(data, "session.db"))); !strings.HasPrefix(got, "SQLite format 3") {
		t.Fatalf("session.db = %q", got[:16])
	}
}

func TestSnapshotUnsafeVersion(t *testing.T) {
	data := t.TempDir()
	for _, v := range []string{"", "../x", ".", "a/b", "-flag"} {
		if _, err := Snapshot(data, v); err == nil {
			t.Errorf("Snapshot accepted version %q", v)
		}
		if err := Restore(data, v); err == nil {
			t.Errorf("Restore accepted version %q", v)
		}
	}
}

func TestRestoreWithoutSnapshot(t *testing.T) {
	if err := Restore(t.TempDir(), "v9.9.9"); err == nil {
		t.Fatal("Restore without a snapshot must fail")
	}
}

func TestRestoreSkipsSnapshotDir(t *testing.T) {
	// A snapshot directory that exists but holds nothing useful.
	data := t.TempDir()
	if err := os.MkdirAll(filepath.Join(data, "rollback", "v0.1.0"), 0o700); err != nil {
		t.Fatal(err)
	}
	if err := Restore(data, "v0.1.0"); err == nil {
		t.Fatal("Restore of an empty snapshot must fail")
	}
}
