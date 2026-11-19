package netmgr

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

func TestWifiStoreSaveAndList(t *testing.T) {
	s, err := OpenWifiStore(filepath.Join(t.TempDir(), "wifi.json"))
	if err != nil {
		t.Fatal(err)
	}
	if err := s.Save("home-wifi", "correct horse battery staple"); err != nil {
		t.Fatal(err)
	}
	if err := s.Save("Café☕", "pässwörd"); err != nil {
		t.Fatal(err)
	}
	nets := s.List()
	if len(nets) != 2 {
		t.Fatalf("nets = %d", len(nets))
	}
	// Most recently saved first.
	if nets[0].SSID != "Café☕" || nets[1].SSID != "home-wifi" {
		t.Fatalf("order = %+v", nets)
	}
}

func TestWifiStorePSKMatchesWPAPassphrase(t *testing.T) {
	s, err := OpenWifiStore(filepath.Join(t.TempDir(), "wifi.json"))
	if err != nil {
		t.Fatal(err)
	}
	if err := s.Save("home-wifi", "correct horse battery staple"); err != nil {
		t.Fatal(err)
	}
	if err := s.Save("Café☕", "pässwörd"); err != nil {
		t.Fatal(err)
	}
	nets := s.List()
	want := map[string]string{
		"home-wifi": "f6d02111aaaf20c1ee53179f99ebaf3a63dadc857cb5aed178e600c73b856620",
		"Café☕":     "bee4aa2e499740ff2ead5a4b6b301adea33db84903a559fa466349f906de6889",
	}
	for _, n := range nets {
		if n.PSK != want[n.SSID] {
			t.Errorf("%q psk = %s, want %s", n.SSID, n.PSK, want[n.SSID])
		}
	}
}

func TestWifiStoreNeverStoresPassphrase(t *testing.T) {
	path := filepath.Join(t.TempDir(), "wifi.json")
	s, _ := OpenWifiStore(path)
	_ = s.Save("home-wifi", "correct horse battery staple")
	data, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	if strings.Contains(string(data), "correct horse") || strings.Contains(string(data), "battery") {
		t.Fatalf("passphrase leaked to disk: %s", data)
	}
}

func TestWifiStoreMarkOKMovesToFront(t *testing.T) {
	s, _ := OpenWifiStore(filepath.Join(t.TempDir(), "wifi.json"))
	_ = s.Save("a", "pw")
	_ = s.Save("b", "pw")
	_ = s.Save("c", "pw")
	if err := s.MarkOK("a", time.Now()); err != nil {
		t.Fatal(err)
	}
	nets := s.List()
	if nets[0].SSID != "a" {
		t.Fatalf("after MarkOK order = %+v", nets)
	}
}

func TestWifiStoreForgetAndErase(t *testing.T) {
	path := filepath.Join(t.TempDir(), "wifi.json")
	s, _ := OpenWifiStore(path)
	_ = s.Save("a", "pw")
	_ = s.Save("b", "pw")
	_ = s.Forget("a")
	if nets := s.List(); len(nets) != 1 || nets[0].SSID != "b" {
		t.Fatalf("after forget = %+v", nets)
	}
	if err := s.Erase(); err != nil {
		t.Fatal(err)
	}
	if len(s.List()) != 0 {
		t.Fatal("after erase not empty")
	}
	if _, err := os.Stat(path); !os.IsNotExist(err) {
		t.Fatal("file still exists after erase")
	}
}

func TestWifiStoreReloadsFromDisk(t *testing.T) {
	path := filepath.Join(t.TempDir(), "wifi.json")
	s, _ := OpenWifiStore(path)
	_ = s.Save("persisted", "pw")
	s2, err := OpenWifiStore(path)
	if err != nil {
		t.Fatal(err)
	}
	if nets := s2.List(); len(nets) != 1 || nets[0].SSID != "persisted" {
		t.Fatalf("reload = %+v", nets)
	}
}
