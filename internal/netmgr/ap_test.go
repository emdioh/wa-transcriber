package netmgr

import (
	"context"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

func writeWatTxt(t *testing.T, content string) string {
	t.Helper()
	p := filepath.Join(t.TempDir(), "wat.txt")
	if err := os.WriteFile(p, []byte(content), 0o644); err != nil {
		t.Fatal(err)
	}
	return p
}

func testAP(t *testing.T, r *fakeRunner) *AP {
	a := NewAP("wlan0", testMAC(), r, testLogger())
	a.WatTxtPath = writeWatTxt(t, "setup_ap_key=secret-pass\ncountry=IT\n")
	a.HostapdConfPath = filepath.Join(t.TempDir(), "hostapd.conf")
	return a
}

func TestHostapdConfGolden(t *testing.T) {
	a := testAP(t, newFakeRunner(nil))
	if err := a.WriteHostapdConf(); err != nil {
		t.Fatal(err)
	}
	got, err := os.ReadFile(a.HostapdConfPath)
	if err != nil {
		t.Fatal(err)
	}
	want := readTestdata(t, "hostapd.conf")
	if string(got) != string(want) {
		t.Fatalf("hostapd.conf differs:\n--- got ---\n%s\n--- want ---\n%s", got, want)
	}
}

func TestSetupKeyFallbackAndCountryDefault(t *testing.T) {
	a := NewAP("wlan0", testMAC(), newFakeRunner(nil), testLogger())
	a.WatTxtPath = filepath.Join(t.TempDir(), "missing.txt")
	if got := a.SetupKey(); got != "wa-setup-001122" {
		t.Fatalf("fallback key = %q", got)
	}
	if got := a.CountryCode(); got != "IT" {
		t.Fatalf("country = %q", got)
	}
}

func TestSetupKeyTooShortFallsBack(t *testing.T) {
	a := NewAP("wlan0", testMAC(), newFakeRunner(nil), testLogger())
	a.WatTxtPath = writeWatTxt(t, "setup_ap_key=short\n")
	if got := a.SetupKey(); got != "wa-setup-001122" {
		t.Fatalf("key = %q", got)
	}
}

func TestAPStations(t *testing.T) {
	r := newFakeRunner(nil)
	r.outputs["iw"] = []string{string(readTestdata(t, "station_dump.txt"))}
	a := testAP(t, r)
	n, err := a.Stations(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	if n != 2 {
		t.Fatalf("stations = %d", n)
	}
}

func TestAPStartCommandSequence(t *testing.T) {
	r := newFakeRunner(nil)
	a := testAP(t, r)
	if err := a.Start(context.Background()); err != nil {
		t.Fatal(err)
	}
	calls := r.snapshot()
	want := []string{
		"ip addr flush dev wlan0",
		"ip addr add 192.168.4.1/24 dev wlan0",
		"ip link set wlan0 up",
		"hostapd " + a.HostapdConfPath,
		"dnsmasq --keep-in-foreground --no-resolv --no-hosts --interface=wlan0 --bind-interfaces --dhcp-range=192.168.4.10,192.168.4.100,255.255.255.0,1h --address=/#/192.168.4.1 --dhcp-option=114,http://192.168.4.1/",
	}
	if len(calls) != len(want) {
		t.Fatalf("calls = %v, want %v", calls, want)
	}
	for i := range want {
		if calls[i] != want[i] {
			t.Errorf("call %d = %q, want %q", i, calls[i], want[i])
		}
	}
	// Stop flushes the address again.
	if err := a.Stop(context.Background()); err != nil {
		t.Fatal(err)
	}
	if last := r.snapshot()[len(r.snapshot())-1]; !strings.Contains(last, "ip addr flush dev wlan0") {
		t.Errorf("last call after Stop = %q", last)
	}
}
