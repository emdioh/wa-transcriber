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
	ops := newFakeOps()
	dhcp := newFakeDHCP(true)
	srv := &fakeAPServer{}

	m := newManaged(mustWifiStore(t), func() []status.Addr { return nil }, func() bool { return false }, testLogger())
	m.base = testSysfs(t)
	m.run = run
	m.ops = ops
	m.dhcp = dhcp.fn()
	m.startServer = srv.start()
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
	opsCalls := strings.Join(ops.snapshot(), "\n")
	runCalls := strings.Join(run.snapshot(), "\n")
	if !strings.Contains(opsCalls, "addr add 192.168.4.1/24 dev wlan0") ||
		!strings.Contains(runCalls, "hostapd ") {
		t.Fatalf("setup AP not started:\nops:\n%s\nrun:\n%s", opsCalls, runCalls)
	}
	if len(srv.snapshot()) != 1 {
		t.Fatalf("apsrv starts = %d, want 1", len(srv.snapshot()))
	}
	for _, c := range run.snapshot() {
		if !strings.HasPrefix(c, "hostapd ") {
			t.Fatalf("unexpected external command: %q", c)
		}
	}
}

func TestManagedStepConnectsToSavedNetwork(t *testing.T) {
	run := newFakeRunner(nil)
	ops := newFakeOps()
	dhcp := newFakeDHCP(true, true)
	ctrl := newFakeWPACtrl()
	ctrl.replies["STATUS"] = [][]byte{readTestdata(t, "wpa_status.txt")}

	store := mustWifiStore(t)
	if err := store.Save("home-wifi", "secret pass"); err != nil {
		t.Fatal(err)
	}
	m := newManaged(store, func() []status.Addr { return nil }, func() bool { return true }, testLogger())
	m.base = testSysfs(t)
	m.run = run
	m.ops = ops
	m.dhcp = dhcp.fn()
	m.wpaCtrl = func(_, _ string) WPACtrl { return ctrl }
	m.timer = func(time.Duration) <-chan time.Time { return make(chan time.Time) }
	now := time.Unix(1700000000, 0)
	m.now = func() time.Time { return now }
	if err := m.discover(); err != nil {
		t.Fatal(err)
	}
	for _, u := range m.wifis {
		u.client.ConfPath = filepath.Join(t.TempDir(), "wpa.conf")
		u.client.CtrlDir = filepath.Join(t.TempDir(), "wpa")
		u.client.PollInterval = time.Millisecond
	}

	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()

	var wg sync.WaitGroup
	wg.Add(1)
	go func() { defer wg.Done(); m.step(ctx); time.Sleep(50 * time.Millisecond) }()
	wg.Wait()

	calls := strings.Join(run.snapshot(), "\n")
	if !strings.Contains(calls, "wpa_supplicant -i wlan0 -D nl80211 -c ") {
		t.Fatalf("wpa_supplicant not started:\n%s", calls)
	}
	for _, c := range run.snapshot() {
		if !strings.HasPrefix(c, "wpa_supplicant ") {
			t.Fatalf("unexpected external command: %q", c)
		}
	}
	dhcp.waitFor(t, "dhcp wlan0")
}

func TestManagedStepPolicyNever(t *testing.T) {
	run := newFakeRunner(nil)
	ops := newFakeOps()
	dhcp := newFakeDHCP(true)
	srv := &fakeAPServer{}

	m := newManaged(mustWifiStore(t), func() []status.Addr { return nil }, func() bool { return false }, testLogger())
	m.base = testSysfs(t)
	m.run = run
	m.ops = ops
	m.dhcp = dhcp.fn()
	m.startServer = srv.start()
	m.timer = func(time.Duration) <-chan time.Time { return make(chan time.Time) }
	m.now = func() time.Time { return time.Now() }
	if err := m.SetAPPolicy(APNever); err != nil {
		t.Fatal(err)
	}
	if err := m.discover(); err != nil {
		t.Fatal(err)
	}
	for _, u := range m.wifis {
		u.ap.HostapdConfPath = filepath.Join(t.TempDir(), "hostapd.conf")
		u.ap.WatTxtPath = filepath.Join(t.TempDir(), "wat.txt")
	}
	m.step(context.Background())
	for _, c := range run.snapshot() {
		if strings.Contains(c, "hostapd") {
			t.Fatalf("hostapd started with policy never: %v", run.snapshot())
		}
	}
	if len(srv.snapshot()) != 0 {
		t.Fatalf("apsrv started with policy never: %v", srv.snapshot())
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
	run := newFakeRunner(nil)
	ops := newFakeOps()
	ctrl := newFakeWPACtrl()
	ctrl.replies["STATUS"] = [][]byte{readTestdata(t, "wpa_status.txt")}

	m := newManaged(store, func() []status.Addr {
		return []status.Addr{{Interface: "eth0", IP: "10.0.0.5"}}
	}, func() bool { return true }, testLogger())
	m.base = testSysfs(t)
	m.run = run
	m.ops = ops
	m.wpaCtrl = func(_, _ string) WPACtrl { return ctrl }
	if err := m.discover(); err != nil {
		t.Fatal(err)
	}
	for _, u := range m.wifis {
		u.client.ConfPath = filepath.Join(t.TempDir(), "wpa.conf")
		u.client.CtrlDir = filepath.Join(t.TempDir(), "wpa")
		u.state = stateConnected
	}

	st := m.Status()
	byName := map[string]Interface{}
	for _, i := range st.Interfaces {
		byName[i.Name] = i
	}
	if byName["eth0"].IPs == nil || byName["eth0"].IPs[0] != "10.0.0.5" {
		t.Fatalf("eth0 = %+v", byName["eth0"])
	}
	if byName["wlan0"].SSID != "home-wifi" {
		t.Fatalf("wlan0 SSID = %q, want home-wifi", byName["wlan0"].SSID)
	}
	if len(run.snapshot()) != 0 {
		t.Fatalf("Status invoked external commands: %v", run.snapshot())
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
// lifts the link. The first poll must therefore drive Ethernet.Up (LinkUp,
// then DHCP) even when the carrier reads 0, or the BeagleBone Black never gets
// a DHCP client running at all.
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

	run := newFakeRunner(nil)
	ops := newFakeOps()
	dhcp := newFakeDHCP(true)
	m := newManaged(nil, func() []status.Addr { return nil }, func() bool { return true }, testLogger())
	m.base = base
	m.run = run
	m.ops = ops
	m.dhcp = dhcp.fn()
	if err := m.discover(); err != nil {
		t.Fatal(err)
	}

	m.step(context.Background())
	dhcpCalls := dhcp.waitFor(t, "dhcp eth0")
	opsCalls := strings.Join(ops.snapshot(), "\n")
	if !strings.Contains(opsCalls, "link up eth0") {
		t.Errorf("the first poll did not lift the link:\n%s", opsCalls)
	}
	if strings.Contains(opsCalls, "addr flush") {
		t.Errorf("the first poll flushed addresses instead of bringing the link up:\n%s", opsCalls)
	}
	if len(run.snapshot()) != 0 {
		t.Errorf("ethernet exec'd external commands: %v", run.snapshot())
	}

	// The next poll sees the same carrier: no second run.
	m.step(context.Background())
	time.Sleep(50 * time.Millisecond)
	if got := len(dhcp.snapshot()); got != len(dhcpCalls) {
		t.Errorf("a repeated poll re-ran DHCP: %v", dhcp.snapshot())
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

	run := newFakeRunner(nil)
	ops := newFakeOps()
	dhcp := newFakeDHCP(true, true, true)
	m := newManaged(nil, func() []status.Addr { return nil }, func() bool { return true }, testLogger())
	m.base = base
	m.run = run
	m.ops = ops
	m.dhcp = dhcp.fn()
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

	opsCalls := strings.Join(ops.snapshot(), "\n")
	if strings.Contains(opsCalls, "addr flush") {
		t.Fatalf("the boot flap flushed the address:\n%s", opsCalls)
	}
	if strings.Contains(opsCalls, "link down") {
		t.Fatalf("the boot flap took the link down:\n%s", opsCalls)
	}
	if len(dhcp.snapshot()) != 3 {
		t.Fatalf("dhcp did not run on every carrier-up event: %v", dhcp.snapshot())
	}
	if len(run.snapshot()) != 0 {
		t.Fatalf("ethernet exec'd external commands: %v", run.snapshot())
	}
}
