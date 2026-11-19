package netmgr

import (
	"context"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/emdioh/wa-transcriber/internal/status"
)

// testSysfs builds an /sys-like tree with eth0 + wlan0 and points a managed
// manager at it.
func testSysfs(t *testing.T) string {
	t.Helper()
	base := filepath.Join(t.TempDir(), "net")
	for _, name := range []string{"eth0", "wlan0"} {
		dir := filepath.Join(base, name)
		if err := os.MkdirAll(dir, 0o755); err != nil {
			t.Fatal(err)
		}
		if err := os.Symlink(filepath.Join(base, "devices", name), filepath.Join(dir, "device")); err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(filepath.Join(dir, "carrier"), []byte("1\n"), 0o644); err != nil {
			t.Fatal(err)
		}
	}
	if err := os.MkdirAll(filepath.Join(base, "wlan0", "phy80211"), 0o755); err != nil {
		t.Fatal(err)
	}
	return base
}

func TestManagedStepStartsSetupAP(t *testing.T) {
	run := newFakeRunner(nil)
	m := newManaged(mustWifiStore(t), func() []status.Addr { return nil }, func() bool { return false }, testLogger())
	m.base = testSysfs(t)
	m.run = run
	m.timer = func(time.Duration) <-chan time.Time { return make(chan time.Time) }
	m.now = func() time.Time { return time.Unix(1700000000, 0) }
	if err := m.discover(); err != nil {
		t.Fatal(err)
	}
	for _, u := range m.wifis {
		u.ap.HostapdConfPath = filepath.Join(t.TempDir(), "hostapd.conf")
		u.ap.WatTxtPath = filepath.Join(t.TempDir(), "wat.txt")
	}

	// No saved networks, not paired -> the setup AP must come up.
	m.step(context.Background())
	calls := strings.Join(run.snapshot(), "\n")
	if !strings.Contains(calls, "ip addr add 192.168.4.1/24 dev wlan0") ||
		!strings.Contains(calls, "hostapd ") {
		t.Fatalf("setup AP not started:\n%s", calls)
	}
}

func TestManagedStepConnectsToSavedNetwork(t *testing.T) {
	run := newFakeRunner(nil)
	store := mustWifiStore(t)
	if err := store.Save("home-wifi", "secret pass"); err != nil {
		t.Fatal(err)
	}
	m := newManaged(store, func() []status.Addr { return nil }, func() bool { return true }, testLogger())
	m.base = testSysfs(t)
	m.run = run
	m.timer = func(time.Duration) <-chan time.Time { return make(chan time.Time) }
	now := time.Unix(1700000000, 0)
	m.now = func() time.Time { return now }
	if err := m.discover(); err != nil {
		t.Fatal(err)
	}
	run.outputs["wpa_cli"] = []string{string(readTestdata(t, "wpa_status.txt"))}

	var wg sync.WaitGroup
	wg.Add(1)
	go func() { defer wg.Done(); m.step(context.Background()); time.Sleep(50 * time.Millisecond) }()
	wg.Wait()

	calls := strings.Join(run.snapshot(), "\n")
	if !strings.Contains(calls, "wpa_supplicant -i wlan0 -D nl80211 -c /run/wat/wpa.conf") {
		t.Fatalf("wpa_supplicant not started:\n%s", calls)
	}
}

func TestManagedStepPolicyNever(t *testing.T) {
	run := newFakeRunner(nil)
	m := newManaged(mustWifiStore(t), func() []status.Addr { return nil }, func() bool { return false }, testLogger())
	m.base = testSysfs(t)
	m.run = run
	m.timer = func(time.Duration) <-chan time.Time { return make(chan time.Time) }
	m.now = func() time.Time { return time.Now() }
	for _, u := range m.wifis {
		u.ap.HostapdConfPath = filepath.Join(t.TempDir(), "hostapd.conf")
		u.ap.WatTxtPath = filepath.Join(t.TempDir(), "wat.txt")
	}
	if err := m.SetAPPolicy(APNever); err != nil {
		t.Fatal(err)
	}
	if err := m.discover(); err != nil {
		t.Fatal(err)
	}
	m.step(context.Background())
	for _, c := range run.snapshot() {
		if strings.Contains(c, "hostapd") {
			t.Fatalf("hostapd started with policy never: %v", run.snapshot())
		}
	}
}

func TestManagedSetAPPolicyValidates(t *testing.T) {
	m := newManaged(nil, func() []status.Addr { return nil }, func() bool { return true }, testLogger())
	if err := m.SetAPPolicy("bogus"); err == nil {
		t.Fatal("accepted a bogus policy")
	}
	for _, p := range []APPolicy{APAuto, APAlways, APNever} {
		if err := m.SetAPPolicy(p); err != nil {
			t.Errorf("%q: %v", p, err)
		}
	}
}

func TestManagedStatusAndSaveForget(t *testing.T) {
	store := mustWifiStore(t)
	m := newManaged(store, func() []status.Addr {
		return []status.Addr{{Interface: "eth0", IP: "10.0.0.5"}}
	}, func() bool { return true }, testLogger())
	m.base = testSysfs(t)
	m.run = newFakeRunner(nil)
	if err := m.discover(); err != nil {
		t.Fatal(err)
	}
	st := m.Status()
	byName := map[string]Interface{}
	for _, i := range st.Interfaces {
		byName[i.Name] = i
	}
	if byName["eth0"].IPs == nil || byName["eth0"].IPs[0] != "10.0.0.5" {
		t.Fatalf("eth0 = %+v", byName["eth0"])
	}
	if err := m.SaveNetwork("home", "secret pass"); err != nil {
		t.Fatal(err)
	}
	if len(store.List()) != 1 {
		t.Fatalf("saved = %+v", store.List())
	}
	if err := m.ForgetNetwork("home"); err != nil {
		t.Fatal(err)
	}
	if len(store.List()) != 0 {
		t.Fatalf("after forget = %+v", store.List())
	}
	if err := m.Erase(); err != nil {
		t.Fatal(err)
	}
}
