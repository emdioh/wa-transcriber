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

// A freshly booted board has eth0 administratively down, so its carrier file
// reads 0 and no later false->true transition ever happens unless somebody
// lifts the link. The first poll must therefore drive Ethernet.Up (ip link
// set up, then DHCP) even when the carrier reads 0, or the BeagleBone Black
// never gets a DHCP client running at all.
func TestManagedFirstPollDrivesEthernetWithCarrierDown(t *testing.T) {
	base := filepath.Join(t.TempDir(), "net")
	for _, name := range []string{"eth0"} {
		dir := filepath.Join(base, name)
		if err := os.MkdirAll(dir, 0o755); err != nil {
			t.Fatal(err)
		}
		if err := os.Symlink(filepath.Join(base, "devices", name), filepath.Join(dir, "device")); err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(filepath.Join(dir, "carrier"), []byte("0\n"), 0o644); err != nil {
			t.Fatal(err)
		}
	}

	run := newFakeRunner(map[string][]error{"udhcpc": {nil}})
	m := newManaged(nil, func() []status.Addr { return nil }, func() bool { return true }, testLogger())
	m.base = base
	m.run = run
	if err := m.discover(); err != nil {
		t.Fatal(err)
	}

	m.step(context.Background())
	calls := run.waitFor(t, "udhcpc")
	joined := strings.Join(calls, "\n")
	if !strings.Contains(joined, "ip link set eth0 up") {
		t.Errorf("the first poll did not lift the link:\n%s", joined)
	}
	if !strings.Contains(joined, "udhcpc -n -i eth0 -f -q -t 5 -T 3 -s /usr/share/udhcpc/default.script") {
		t.Errorf("the first poll did not run the DHCP client:\n%s", joined)
	}
	if strings.Contains(joined, "ip addr flush") {
		t.Errorf("the first poll flushed addresses instead of bringing the link up:\n%s", joined)
	}

	// The next poll sees the same carrier: no second run.
	m.step(context.Background())
	time.Sleep(50 * time.Millisecond)
	if got := len(run.snapshot()); got != len(calls) {
		t.Errorf("a repeated poll re-ran commands: %v", run.snapshot())
	}
}

// The board log shows carrier up (first) at 19.3 s, a lease at 22.6 s, then
// carrier down at 23.3 s: a flap right after boot, while pairing sockets are
// being opened. The managed loop must not flush the address on that down
// event, so the address and its sockets survive until DHCP returns.
func TestManagedCarrierFlapKeepsEthernetAddress(t *testing.T) {
	base := filepath.Join(t.TempDir(), "net")
	dir := filepath.Join(base, "eth0")
	if err := os.MkdirAll(dir, 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.Symlink(filepath.Join(base, "devices", "eth0"), filepath.Join(dir, "device")); err != nil {
		t.Fatal(err)
	}
	carrier := filepath.Join(dir, "carrier")
	if err := os.WriteFile(carrier, []byte("0\n"), 0o644); err != nil {
		t.Fatal(err)
	}

	run := newFakeRunner(map[string][]error{"udhcpc": {nil, nil}})
	m := newManaged(nil, func() []status.Addr { return nil }, func() bool { return true }, testLogger())
	m.base = base
	m.run = run
	if err := m.discover(); err != nil {
		t.Fatal(err)
	}
	step := func() {
		m.step(context.Background())
		// stepEthernet runs Up/Down in a goroutine: wait for it to finish.
		deadline := time.Now().Add(3 * time.Second)
		for time.Now().Before(deadline) {
			m.mu.Lock()
			busy := m.eths["eth0"].busy
			m.mu.Unlock()
			if !busy {
				return
			}
			time.Sleep(2 * time.Millisecond)
		}
		t.Fatal("ethernet step never finished")
	}

	step() // first poll, carrier 0: still brings the link up
	if err := os.WriteFile(carrier, []byte("1\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	step() // carrier up
	if err := os.WriteFile(carrier, []byte("0\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	step() // carrier down (the boot flap)
	if err := os.WriteFile(carrier, []byte("1\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	step() // carrier back up: DHCP again

	calls := strings.Join(run.snapshot(), "\n")
	if strings.Contains(calls, "ip addr flush") {
		t.Fatalf("the boot flap flushed the address:\n%s", calls)
	}
	if !strings.Contains(calls, "ip link set eth0 down") && strings.Count(calls, "udhcpc ") != 3 {
		t.Fatalf("dhcp did not run on every carrier-up event:\n%s", calls)
	}
}
