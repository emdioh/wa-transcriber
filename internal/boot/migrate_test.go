package boot

import (
	"context"
	"errors"
	"log/slog"
	"os"
	"path/filepath"
	"reflect"
	"strings"
	"testing"
)

type migrateHarness struct {
	t        *testing.T
	root     string
	bootDir  string
	dataDir  string
	devDir   string
	mntDir   string
	events   []string
	mountErr func(source, target, fstype string, flags uintptr, data string) error
	umntErr  func(target string, flags int) error
}

func newMigrateHarness(t *testing.T) *migrateHarness {
	t.Helper()
	root := t.TempDir()
	h := &migrateHarness{
		t:       t,
		root:    root,
		bootDir: filepath.Join(root, "boot"),
		dataDir: filepath.Join(root, "data"),
		devDir:  filepath.Join(root, "dev"),
		mntDir:  filepath.Join(root, "run", "wat-migrate"),
	}
	for _, d := range []string{h.bootDir, h.dataDir, h.devDir} {
		if err := os.MkdirAll(d, 0o755); err != nil {
			t.Fatal(err)
		}
	}
	return h
}

func (h *migrateHarness) options() Options {
	return Options{
		Log:             slog.New(slog.NewTextHandler(&strings.Builder{}, &slog.HandlerOptions{Level: slog.LevelError})),
		BootDir:         h.bootDir,
		DataDir:         h.dataDir,
		DevDir:          h.devDir + "/",
		MigrateMountDir: h.mntDir,
		Mount: func(source, target, fstype string, flags uintptr, data string) error {
			switch {
			case target == h.bootDir && flags&msRemount != 0 && flags&msReadOnly == 0:
				h.events = append(h.events, "remount-rw")
			case target == h.bootDir && flags&msRemount != 0 && flags&msReadOnly != 0:
				h.events = append(h.events, "remount-ro")
			default:
				h.events = append(h.events, "mount:"+source+"->"+target)
			}
			if h.mountErr != nil {
				return h.mountErr(source, target, fstype, flags, data)
			}
			return nil
		},
		Unmount: func(target string, flags int) error {
			h.events = append(h.events, "unmount:"+target)
			if h.umntErr != nil {
				return h.umntErr(target, flags)
			}
			return nil
		},
		removeFile: func(path string) error {
			h.events = append(h.events, "rm:"+filepath.Base(path))
			return os.Remove(path)
		},
		removeAll: func(path string) error {
			h.events = append(h.events, "rm-rf:"+filepath.Base(path))
			return os.RemoveAll(path)
		},
	}
}

func TestMigrateNoMarkerIsNoOp(t *testing.T) {
	h := newMigrateHarness(t)
	o := h.options()

	o.migrateFromMarker(context.Background())

	if len(h.events) != 0 {
		t.Fatalf("events = %v, want none when marker is absent", h.events)
	}
	entries, err := os.ReadDir(h.dataDir)
	if err != nil {
		t.Fatal(err)
	}
	if len(entries) != 0 {
		t.Fatalf("dataDir entries = %v, want empty", entries)
	}
}

func TestMigrateStagedFilesAndCleanupOrder(t *testing.T) {
	h := newMigrateHarness(t)
	marker := filepath.Join(h.bootDir, migrateMarkerName)
	stagedDir := filepath.Join(h.bootDir, migrateStagedName)

	writeFile(t, marker, []byte("wat-migrate 1\nfrom /dev/mmcblk0p4\nfile config.json\nfile session.db\nfile session.db-wal\nfile wifi.json\n"))
	writeFile(t, filepath.Join(stagedDir, "config.json"), []byte(`{"pin":"1234"}`))
	writeFile(t, filepath.Join(stagedDir, "session.db"), []byte("sqlite-bytes"))
	writeFile(t, filepath.Join(stagedDir, "session.db-wal"), []byte("wal-bytes"))
	// wifi.json intentionally absent (normal when device had no WiFi config).

	o := h.options()
	h.mountErr = func(_, target, _ string, flags uintptr, _ string) error {
		if target != h.bootDir {
			t.Fatalf("unexpected mount target %q", target)
		}
		if flags&msRemount != 0 && flags&msReadOnly == 0 {
			// Before remount rw: files must already be copied and nothing deleted yet.
			if got := string(mustRead(t, filepath.Join(h.dataDir, "config.json"))); got != `{"pin":"1234"}` {
				t.Errorf("config.json before remount rw = %q", got)
			}
			if _, err := os.Stat(marker); err != nil {
				t.Errorf("marker missing before remount rw: %v", err)
			}
			if _, err := os.Stat(stagedDir); err != nil {
				t.Errorf("stagedDir missing before remount rw: %v", err)
			}
		}
		if flags&msRemount != 0 && flags&msReadOnly != 0 {
			// Before remount ro: both marker and stagedDir must already be gone.
			if _, err := os.Stat(marker); !errors.Is(err, os.ErrNotExist) {
				t.Errorf("marker still present at remount ro: %v", err)
			}
			if _, err := os.Stat(stagedDir); !errors.Is(err, os.ErrNotExist) {
				t.Errorf("stagedDir still present at remount ro: %v", err)
			}
		}
		return nil
	}

	o.migrateFromMarker(context.Background())

	wantFiles := map[string]string{
		"config.json":    `{"pin":"1234"}`,
		"session.db":     "sqlite-bytes",
		"session.db-wal": "wal-bytes",
	}
	for name, want := range wantFiles {
		got := string(mustRead(t, filepath.Join(h.dataDir, name)))
		if got != want {
			t.Errorf("%s = %q, want %q", name, got, want)
		}
		if _, err := os.Stat(filepath.Join(h.dataDir, "."+name+".migrating")); !errors.Is(err, os.ErrNotExist) {
			t.Errorf("leftover temp file for %s: %v", name, err)
		}
	}
	if _, err := os.Stat(filepath.Join(h.dataDir, "wifi.json")); !errors.Is(err, os.ErrNotExist) {
		t.Errorf("wifi.json should not exist: %v", err)
	}

	wantEvents := []string{"remount-rw", "rm:wat-migrate", "rm-rf:wat-migrate.d", "remount-ro"}
	if !reflect.DeepEqual(h.events, wantEvents) {
		t.Fatalf("events = %v, want %v", h.events, wantEvents)
	}

	// Second run is a no-op once the marker is gone.
	h.events = nil
	o.migrateFromMarker(context.Background())
	if len(h.events) != 0 {
		t.Fatalf("second run events = %v, want none", h.events)
	}
}

func TestMigrateSkipsUnsafeNames(t *testing.T) {
	h := newMigrateHarness(t)
	marker := filepath.Join(h.bootDir, migrateMarkerName)
	stagedDir := filepath.Join(h.bootDir, migrateStagedName)

	writeFile(t, marker, []byte(
		"wat-migrate 1\n"+
			"file ../x\n"+
			"file a/b\n"+
			"file .\n"+
			"file ..\n"+
			"file \n"+
			"file safe.json\n"))
	writeFile(t, filepath.Join(stagedDir, "safe.json"), []byte("ok"))
	writeFile(t, filepath.Join(stagedDir, "a", "b"), []byte("nested"))
	writeFile(t, filepath.Join(h.bootDir, "x"), []byte("outside"))

	o := h.options()
	o.migrateFromMarker(context.Background())

	if got := string(mustRead(t, filepath.Join(h.dataDir, "safe.json"))); got != "ok" {
		t.Fatalf("safe.json = %q, want ok", got)
	}
	if _, err := os.Stat(filepath.Join(h.root, "x")); !errors.Is(err, os.ErrNotExist) {
		t.Fatalf("../x escaped dataDir: %v", err)
	}
	if _, err := os.Stat(filepath.Join(h.dataDir, "a")); !errors.Is(err, os.ErrNotExist) {
		t.Fatalf("a/b created inside dataDir: %v", err)
	}
	entries, err := os.ReadDir(h.dataDir)
	if err != nil {
		t.Fatal(err)
	}
	if len(entries) != 1 || entries[0].Name() != "safe.json" {
		t.Fatalf("dataDir entries = %v, want only safe.json", entries)
	}
}

func TestMigrateCopyFailureKeepsMarker(t *testing.T) {
	h := newMigrateHarness(t)
	marker := filepath.Join(h.bootDir, migrateMarkerName)
	stagedDir := filepath.Join(h.bootDir, migrateStagedName)

	writeFile(t, marker, []byte("wat-migrate 1\nfile config.json\nfile session.db\n"))
	writeFile(t, filepath.Join(stagedDir, "config.json"), []byte("cfg"))
	writeFile(t, filepath.Join(stagedDir, "session.db"), []byte("db"))

	// Make <DataDir>/session.db a non-empty directory so rename fails reliably
	// even if tests run as root.
	blocker := filepath.Join(h.dataDir, "session.db")
	writeFile(t, filepath.Join(blocker, "keep"), []byte("x"))

	o := h.options()
	o.migrateFromMarker(context.Background())

	if len(h.events) != 0 {
		t.Fatalf("events = %v, want no remount or removal after copy failure", h.events)
	}
	if _, err := os.Stat(marker); err != nil {
		t.Fatalf("marker was removed after copy failure: %v", err)
	}
	if _, err := os.Stat(filepath.Join(stagedDir, "session.db")); err != nil {
		t.Fatalf("staged files were removed after copy failure: %v", err)
	}
	if _, err := os.Stat(filepath.Join(h.dataDir, ".session.db.migrating")); !errors.Is(err, os.ErrNotExist) {
		t.Fatalf("temp file left behind after rename failure: %v", err)
	}

	// Idempotent retry: once the blocker is cleared, the next run finishes and
	// cleans up the marker.
	if err := os.RemoveAll(blocker); err != nil {
		t.Fatal(err)
	}
	o.migrateFromMarker(context.Background())
	if got := string(mustRead(t, filepath.Join(h.dataDir, "session.db"))); got != "db" {
		t.Fatalf("session.db after retry = %q, want db", got)
	}
	if _, err := os.Stat(marker); !errors.Is(err, os.ErrNotExist) {
		t.Fatalf("marker still exists after successful retry: %v", err)
	}
}

func TestMigrateRemountRWFailureKeepsMarker(t *testing.T) {
	h := newMigrateHarness(t)
	marker := filepath.Join(h.bootDir, migrateMarkerName)
	stagedDir := filepath.Join(h.bootDir, migrateStagedName)

	writeFile(t, marker, []byte("wat-migrate 1\nfile config.json\n"))
	writeFile(t, filepath.Join(stagedDir, "config.json"), []byte("cfg-data"))

	o := h.options()
	h.mountErr = func(_, target, _ string, flags uintptr, _ string) error {
		if target == h.bootDir && flags&msRemount != 0 && flags&msReadOnly == 0 {
			return errors.New("EROFS")
		}
		return nil
	}

	o.migrateFromMarker(context.Background())

	if got := string(mustRead(t, filepath.Join(h.dataDir, "config.json"))); got != "cfg-data" {
		t.Fatalf("config.json = %q, want cfg-data", got)
	}
	if _, err := os.Stat(marker); err != nil {
		t.Fatalf("marker must stay when remount rw fails: %v", err)
	}
	if _, err := os.Stat(stagedDir); err != nil {
		t.Fatalf("stagedDir must stay when remount rw fails: %v", err)
	}
	wantEvents := []string{"remount-rw"}
	if !reflect.DeepEqual(h.events, wantEvents) {
		t.Fatalf("events = %v, want %v", h.events, wantEvents)
	}
}

func TestMigrateFromFallbackDevice(t *testing.T) {
	h := newMigrateHarness(t)
	marker := filepath.Join(h.bootDir, migrateMarkerName)
	stagedDir := filepath.Join(h.bootDir, migrateStagedName)

	writeFile(t, marker, []byte("wat-migrate 1\nfrom /dev/mmcblk0p4\nfile config.json\nfile session.db\n"))
	// config.json is staged (must be preferred); session.db is only on the `from` device.
	writeFile(t, filepath.Join(stagedDir, "config.json"), []byte("staged-config"))
	writeFile(t, filepath.Join(h.devDir, "mmcblk0p4"), ext4Image(DataLabel))

	o := h.options()
	h.mountErr = func(source, target, fstype string, flags uintptr, _ string) error {
		if target == h.mntDir {
			if source != "/dev/mmcblk0p4" {
				t.Errorf("from mount source = %q, want /dev/mmcblk0p4", source)
			}
			if fstype != "ext4" {
				t.Errorf("from mount fstype = %q, want ext4", fstype)
			}
			wantFlags := uintptr(msReadOnly | msNoDev | msNoSuid | msNoAtime)
			if flags != wantFlags {
				t.Errorf("from mount flags = %#x, want %#x", flags, wantFlags)
			}
			writeFile(t, filepath.Join(target, "config.json"), []byte("device-config"))
			writeFile(t, filepath.Join(target, "session.db"), []byte("device-session"))
		}
		return nil
	}
	h.umntErr = func(target string, _ int) error {
		if target == h.mntDir {
			_ = os.Remove(filepath.Join(target, "config.json"))
			_ = os.Remove(filepath.Join(target, "session.db"))
		}
		return nil
	}

	o.migrateFromMarker(context.Background())

	if got := string(mustRead(t, filepath.Join(h.dataDir, "config.json"))); got != "staged-config" {
		t.Fatalf("config.json = %q, want staged-config (staged must win over from)", got)
	}
	if got := string(mustRead(t, filepath.Join(h.dataDir, "session.db"))); got != "device-session" {
		t.Fatalf("session.db = %q, want device-session (from fallback)", got)
	}
	if _, err := os.Stat(marker); !errors.Is(err, os.ErrNotExist) {
		t.Fatalf("marker still exists: %v", err)
	}
	if _, err := os.Stat(stagedDir); !errors.Is(err, os.ErrNotExist) {
		t.Fatalf("stagedDir still exists: %v", err)
	}

	wantEvents := []string{
		"mount:/dev/mmcblk0p4->" + h.mntDir,
		"unmount:" + h.mntDir,
		"remount-rw",
		"rm:wat-migrate",
		"rm-rf:wat-migrate.d",
		"remount-ro",
	}
	if !reflect.DeepEqual(h.events, wantEvents) {
		t.Fatalf("events = %v, want %v", h.events, wantEvents)
	}
}

func TestMigrateFromUnmountFailureKeepsMarker(t *testing.T) {
	h := newMigrateHarness(t)
	marker := filepath.Join(h.bootDir, migrateMarkerName)

	writeFile(t, marker, []byte("wat-migrate 1\nfrom /dev/mmcblk0p4\nfile session.db\n"))
	writeFile(t, filepath.Join(h.devDir, "mmcblk0p4"), ext4Image(DataLabel))

	o := h.options()
	h.mountErr = func(_, target, _ string, _ uintptr, _ string) error {
		if target == h.mntDir {
			writeFile(t, filepath.Join(target, "session.db"), []byte("device-session"))
		}
		return nil
	}
	h.umntErr = func(string, int) error { return errors.New("EBUSY") }

	o.migrateFromMarker(context.Background())

	if got := string(mustRead(t, filepath.Join(h.dataDir, "session.db"))); got != "device-session" {
		t.Fatalf("session.db = %q, want device-session", got)
	}
	if _, err := os.Stat(marker); err != nil {
		t.Fatalf("marker must stay when unmount fails: %v", err)
	}
	wantEvents := []string{
		"mount:/dev/mmcblk0p4->" + h.mntDir,
		"unmount:" + h.mntDir,
	}
	if !reflect.DeepEqual(h.events, wantEvents) {
		t.Fatalf("events = %v, want %v", h.events, wantEvents)
	}
}

func TestMigrateIgnoresUnsafeFromDevice(t *testing.T) {
	for _, bad := range []string{"/etc/passwd", "/dev/../etc/passwd", "mmcblk0p4", "/dev/"} {
		t.Run(bad, func(t *testing.T) {
			h := newMigrateHarness(t)
			marker := filepath.Join(h.bootDir, migrateMarkerName)
			stagedDir := filepath.Join(h.bootDir, migrateStagedName)
			writeFile(t, marker, []byte("wat-migrate 1\nfrom "+bad+"\nfile config.json\n"))
			writeFile(t, filepath.Join(stagedDir, "config.json"), []byte("ok"))

			o := h.options()
			o.migrateFromMarker(context.Background())

			if got := string(mustRead(t, filepath.Join(h.dataDir, "config.json"))); got != "ok" {
				t.Fatalf("config.json = %q, want ok", got)
			}
			for _, ev := range h.events {
				if strings.HasPrefix(ev, "mount:") {
					t.Fatalf("unsafe from %q triggered a mount: %v", bad, h.events)
				}
			}
		})
	}
}

func TestMigrateEmptySourcesRemovesMarker(t *testing.T) {
	h := newMigrateHarness(t)
	marker := filepath.Join(h.bootDir, migrateMarkerName)
	writeFile(t, marker, []byte("wat-migrate 1\nfrom /tmp/not-a-dev\nfile config.json\n"))

	o := h.options()
	o.migrateFromMarker(context.Background())

	if _, err := os.Stat(marker); !errors.Is(err, os.ErrNotExist) {
		t.Fatalf("marker should have been removed: %v", err)
	}
	wantEvents := []string{"remount-rw", "rm:wat-migrate", "remount-ro"}
	if !reflect.DeepEqual(h.events, wantEvents) {
		t.Fatalf("events = %v, want %v", h.events, wantEvents)
	}
}

func TestMountDataTriggersMigration(t *testing.T) {
	m := newFakeMachine(t)
	writeFile(t, filepath.Join(m.devDir, "mmcblk0p4"), ext4Image(DataLabel))

	bootDir := filepath.Join(m.dir, "boot")
	dataDir := filepath.Join(m.dir, "data")
	if err := os.MkdirAll(dataDir, 0o755); err != nil {
		t.Fatal(err)
	}
	marker := filepath.Join(bootDir, migrateMarkerName)
	writeFile(t, marker, []byte("wat-migrate 1\nfile config.json\n"))
	writeFile(t, filepath.Join(bootDir, migrateStagedName, "config.json"), []byte("migrated"))

	o := m.options()
	o.BootDir = bootDir
	o.DataDir = dataDir

	if err := m.mountDataOnly(context.Background(), o); err != nil {
		t.Fatalf("mountData: %v", err)
	}
	if got := string(mustRead(t, filepath.Join(dataDir, "config.json"))); got != "migrated" {
		t.Fatalf("config.json = %q, want migrated", got)
	}
	if _, err := os.Stat(marker); !errors.Is(err, os.ErrNotExist) {
		t.Fatalf("marker still exists after mountData: %v", err)
	}
}
