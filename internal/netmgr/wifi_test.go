package netmgr

import (
	"context"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

func readTestdata(t *testing.T, name string) []byte {
	t.Helper()
	b, err := os.ReadFile(filepath.Join("testdata", name))
	if err != nil {
		t.Fatal(err)
	}
	return b
}

func TestParseWPACliStatus(t *testing.T) {
	state, ssid := parseWPACliStatus(readTestdata(t, "wpa_status.txt"))
	if state != "COMPLETED" || ssid != "home-wifi" {
		t.Fatalf("state=%q ssid=%q", state, ssid)
	}
}

func TestParseScanResults(t *testing.T) {
	nets := parseScanResults(readTestdata(t, "scan_results.txt"))
	if len(nets) != 3 {
		t.Fatalf("nets = %+v", nets)
	}
	if nets[0].SSID != "home-wifi" || nets[0].Signal != -45 || !nets[0].Secured {
		t.Errorf("nets[0] = %+v", nets[0])
	}
	if nets[1].SSID != "office" || nets[1].Signal != -80 || !nets[1].Secured {
		t.Errorf("nets[1] = %+v", nets[1])
	}
	if nets[2].SSID != "cafe-open" || nets[2].Signal != -90 || nets[2].Secured {
		t.Errorf("nets[2] = %+v", nets[2])
	}
}

func TestRenderWPAConf(t *testing.T) {
	s, _ := OpenWifiStore(filepath.Join(t.TempDir(), "wifi.json"))
	_ = s.Save("My WiFi", "secret pass")
	conf := renderWPAConf("/run/wat/wpa", s.List())
	for _, want := range []string{
		"ctrl_interface=/run/wat/wpa\n",
		"update_config=0\n",
		"ssid=4d792057694669", // "My WiFi" hex, no quotes
	} {
		if !strings.Contains(conf, want) {
			t.Fatalf("conf missing %q:\n%s", want, conf)
		}
	}
	if strings.Contains(conf, "secret pass") {
		t.Fatal("passphrase leaked to wpa.conf")
	}
	if !strings.Contains(conf, "priority=1") {
		t.Fatalf("priority missing:\n%s", conf)
	}
}

func TestWifiConnectRunsDHCP(t *testing.T) {
	r := newFakeRunner(nil)
	r.outputs["wpa_cli"] = []string{string(readTestdata(t, "wpa_status.txt"))}
	w := NewWifiClient("wlan0", r, mustWifiStore(t), testLogger())
	w.PollInterval = time.Millisecond
	w.ConnectTimeout = time.Second

	if err := w.Connect(context.Background()); err != nil {
		t.Fatalf("Connect: %v", err)
	}
	calls := strings.Join(r.snapshot(), "\n")
	if !strings.Contains(calls, "wpa_cli -p /run/wat/wpa -i wlan0 status") {
		t.Errorf("status not polled:\n%s", calls)
	}
	if !strings.Contains(calls, "udhcpc -n -i wlan0 -f -q -t 5 -T 3 -s /usr/share/udhcpc/default.script") {
		t.Errorf("dhcp not run:\n%s", calls)
	}
}

func TestWifiConnectTimesOut(t *testing.T) {
	r := newFakeRunner(nil)
	r.outputs["wpa_cli"] = []string{"wpa_state=SCANNING\n"}
	w := NewWifiClient("wlan0", r, mustWifiStore(t), testLogger())
	w.PollInterval = time.Millisecond
	w.ConnectTimeout = time.Millisecond

	if err := w.Connect(context.Background()); err == nil {
		t.Fatal("expected a timeout error")
	}
}

func TestWifiScan(t *testing.T) {
	r := newFakeRunner(nil)
	r.outputs["wpa_cli"] = []string{"OK\n", string(readTestdata(t, "scan_results.txt"))}
	w := NewWifiClient("wlan0", r, mustWifiStore(t), testLogger())

	nets, err := w.Scan(context.Background())
	if err != nil {
		t.Fatalf("Scan: %v", err)
	}
	if len(nets) != 3 || nets[0].SSID != "home-wifi" {
		t.Fatalf("nets = %+v", nets)
	}
	calls := strings.Join(r.snapshot(), "\n")
	if !strings.Contains(calls, "wpa_cli -p /run/wat/wpa -i wlan0 scan\nwpa_cli -p /run/wat/wpa -i wlan0 scan_results") {
		t.Errorf("scan sequence not run:\n%s", calls)
	}
}

func mustWifiStore(t *testing.T) *WifiStore {
	t.Helper()
	s, err := OpenWifiStore(filepath.Join(t.TempDir(), "wifi.json"))
	if err != nil {
		t.Fatal(err)
	}
	return s
}
