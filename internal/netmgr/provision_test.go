package netmgr

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
)

// writeWatTxt puts a wat.txt in a temp dir and returns its path and the data
// directory the store lives in.
func writeProvisionWat(t *testing.T, body string) (watPath, dataDir string) {
	t.Helper()
	dir := t.TempDir()
	watPath = filepath.Join(dir, "wat.txt")
	if err := os.WriteFile(watPath, []byte(body), 0o644); err != nil {
		t.Fatal(err)
	}
	return watPath, filepath.Join(dir, "data")
}

func openStore(t *testing.T, dataDir string) *WifiStore {
	t.Helper()
	if err := os.MkdirAll(dataDir, 0o755); err != nil {
		t.Fatal(err)
	}
	s, err := OpenWifiStore(filepath.Join(dataDir, "wifi.json"))
	if err != nil {
		t.Fatal(err)
	}
	return s
}

func TestWatValueParsing(t *testing.T) {
	// The card is edited on Windows: CRLF, comments, spaces around '='.
	body := "# wifi_ssid=commented-out\r\n" +
		"setup_ap_key=change-me-please\r\n" +
		"  wifi_ssid = Casa WiFi  \r\n" +
		"wifi_passphrase=pa=ss#word\r\n"
	for _, c := range []struct{ key, want string }{
		{"setup_ap_key", "change-me-please"},
		{"wifi_ssid", "Casa WiFi"},
		{"wifi_passphrase", "pa=ss#word"}, // '=' and '#' are part of the value
		{"wifi_ssid_extra", ""},
		{"wifi_ssid", "Casa WiFi"},
	} {
		if got := watValue([]byte(body), c.key); got != c.want {
			t.Errorf("watValue(%q) = %q, want %q", c.key, got, c.want)
		}
	}
}

func TestProvisionWifiSavesTheNetwork(t *testing.T) {
	watPath, dataDir := writeProvisionWat(t, "wifi_ssid=Casa\r\nwifi_passphrase=correct horse\r\n")
	s := openStore(t, dataDir)
	ssid, err := ProvisionWifi(watPath, s)
	if err != nil || ssid != "Casa" {
		t.Fatalf("ProvisionWifi = %q, %v", ssid, err)
	}
	nets := s.List()
	if len(nets) != 1 || nets[0].SSID != "Casa" {
		t.Fatalf("nets = %+v", nets)
	}
	// The PSK is the one wpa_passphrase would derive: the passphrase itself
	// is never stored.
	if nets[0].PSK == "correct horse" || len(nets[0].PSK) != 64 {
		t.Fatalf("PSK = %q", nets[0].PSK)
	}
}

func TestProvisionWifiIsIdempotentAndKeepsTheOrder(t *testing.T) {
	watPath, dataDir := writeProvisionWat(t, "wifi_ssid=Casa\nwifi_passphrase=correct horse\n")
	s := openStore(t, dataDir)
	if _, err := ProvisionWifi(watPath, s); err != nil {
		t.Fatal(err)
	}
	// A network added later from the web UI must stay ahead of the imported
	// one: booting again may not promote wat.txt's network every time.
	if err := s.Save("Later", "another passphrase"); err != nil {
		t.Fatal(err)
	}
	ssid, err := ProvisionWifi(watPath, s)
	if err != nil {
		t.Fatal(err)
	}
	if ssid != "" {
		t.Fatalf("second import reported a change: %q", ssid)
	}
	nets := s.List()
	if len(nets) != 2 || nets[0].SSID != "Later" {
		t.Fatalf("order after re-import = %+v", nets)
	}
}

func TestProvisionWifiAppliesAChangedPassphrase(t *testing.T) {
	watPath, dataDir := writeProvisionWat(t, "wifi_ssid=Casa\nwifi_passphrase=old passphrase\n")
	s := openStore(t, dataDir)
	if _, err := ProvisionWifi(watPath, s); err != nil {
		t.Fatal(err)
	}
	oldPSK := s.List()[0].PSK
	if err := os.WriteFile(watPath, []byte("wifi_ssid=Casa\nwifi_passphrase=new passphrase\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	ssid, err := ProvisionWifi(watPath, s)
	if err != nil || ssid != "Casa" {
		t.Fatalf("ProvisionWifi = %q, %v", ssid, err)
	}
	if got := s.List()[0].PSK; got == oldPSK {
		t.Fatal("the new passphrase was not applied")
	}
}

func TestProvisionWifiRejectsAShortPassphrase(t *testing.T) {
	watPath, dataDir := writeProvisionWat(t, "wifi_ssid=Casa\nwifi_passphrase=short\n")
	s := openStore(t, dataDir)
	_, err := ProvisionWifi(watPath, s)
	if err == nil || !strings.Contains(err.Error(), "8 to 63") {
		t.Fatalf("err = %v", err)
	}
	if len(s.List()) != 0 {
		t.Fatalf("a bad passphrase was saved: %+v", s.List())
	}
}

func TestProvisionWifiWithoutTheKeys(t *testing.T) {
	watPath, dataDir := writeProvisionWat(t, "setup_ap_key=change-me-please\ncountry=IT\n")
	s := openStore(t, dataDir)
	ssid, err := ProvisionWifi(watPath, s)
	if err != nil || ssid != "" || len(s.List()) != 0 {
		t.Fatalf("ProvisionWifi = %q, %v, nets = %+v", ssid, err, s.List())
	}
}

func TestProvisionWifiWithoutAFile(t *testing.T) {
	s := openStore(t, t.TempDir())
	ssid, err := ProvisionWifi(filepath.Join(t.TempDir(), "missing.txt"), s)
	if err != nil || ssid != "" {
		t.Fatalf("ProvisionWifi = %q, %v", ssid, err)
	}
}

func TestHostnameFromWatTxt(t *testing.T) {
	cases := []struct {
		name    string
		body    string
		want    string
		wantErr bool
	}{
		{"custom", "hostname=wabbb\r\n", "wabbb", false},
		{"with local suffix", "hostname=WaBBB.local\n", "wabbb", false},
		{"inline comment", "hostname=wabbb # basement\n", "wabbb", false},
		{"last wins", "hostname=wa\nhostname=wabbb\n", "wabbb", false},
		{"absent key", "country=IT\n", "wa", false},
		{"invalid spaces", "hostname=wa bbb\n", "wa", true},
		{"invalid leading dash", "hostname=-wabbb\n", "wa", true},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			watPath, _ := writeProvisionWat(t, c.body)
			got, err := Hostname(watPath)
			if got != c.want {
				t.Errorf("Hostname() = %q, want %q", got, c.want)
			}
			if (err != nil) != c.wantErr {
				t.Errorf("Hostname() err = %v, wantErr %v", err, c.wantErr)
			}
		})
	}
	got, err := Hostname(filepath.Join(t.TempDir(), "missing.txt"))
	if got != "wa" || err != nil {
		t.Fatalf("missing file: Hostname() = %q, %v; want wa, nil", got, err)
	}
}
