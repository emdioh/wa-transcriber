package netmgr

import (
	"os"
	"path/filepath"
	"testing"
)

// buildSysfs creates a fake /sys/class/net tree and returns its root.
func buildSysfs(t *testing.T) string {
	t.Helper()
	base := filepath.Join(t.TempDir(), "net")
	mkdir := func(name string) string {
		p := filepath.Join(base, name)
		if err := os.MkdirAll(p, 0o755); err != nil {
			t.Fatal(err)
		}
		return p
	}
	write := func(name, content string) {
		if err := os.WriteFile(filepath.Join(base, name), []byte(content), 0o644); err != nil {
			t.Fatal(err)
		}
	}
	mkdir("lo")
	eth := mkdir("eth0")
	if err := os.Symlink(filepath.Join(base, "devices", "eth0"), filepath.Join(eth, "device")); err != nil {
		t.Fatal(err)
	}
	write("eth0/carrier", "1\n")

	wlan := mkdir("wlan0")
	if err := os.Symlink(filepath.Join(base, "devices", "wlan0"), filepath.Join(wlan, "device")); err != nil {
		t.Fatal(err)
	}
	mkdir("wlan0/phy80211")
	write("wlan0/carrier", "1\n")

	wlan2 := mkdir("wlan1")
	if err := os.Symlink(filepath.Join(base, "devices", "wlan1"), filepath.Join(wlan2, "device")); err != nil {
		t.Fatal(err)
	}
	mkdir("wlan1/wireless")
	write("wlan1/carrier", "0\n")

	mkdir("docker0") // virtual: no device symlink
	return base
}

func TestDiscover(t *testing.T) {
	base := buildSysfs(t)
	got, err := Discover(osFS{}, base)
	if err != nil {
		t.Fatalf("Discover: %v", err)
	}
	if len(got) != 3 {
		t.Fatalf("interfaces = %d, want 3: %+v", len(got), got)
	}
	if got[0].Name != "eth0" || got[0].Kind != KindEthernet || !got[0].Carrier {
		t.Errorf("eth0 = %+v", got[0])
	}
	if got[1].Name != "wlan0" || got[1].Kind != KindWiFi || !got[1].Carrier {
		t.Errorf("wlan0 = %+v", got[1])
	}
	if got[2].Name != "wlan1" || got[2].Kind != KindWiFi || got[2].Carrier {
		t.Errorf("wlan1 = %+v", got[2])
	}
}

func TestDiscoverMissingBase(t *testing.T) {
	if _, err := Discover(osFS{}, filepath.Join(t.TempDir(), "missing")); err == nil {
		t.Fatal("expected an error for a missing base directory")
	}
}
