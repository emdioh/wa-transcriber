package netmgr

import (
	"context"
	"net"
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
	ctrl := newFakeWPACtrl()
	ctrl.replies["STATUS"] = [][]byte{readTestdata(t, "wpa_status.txt")}
	dhcp := newFakeDHCP(true)

	w := NewWifiClient("wlan0", testMAC(), r, newFakeOps(), mustWifiStore(t), testLogger())
	w.Ctrl = ctrl
	w.DHCP = dhcp.fn()
	w.PollInterval = time.Millisecond
	w.ConnectTimeout = time.Second

	if err := w.Connect(context.Background()); err != nil {
		t.Fatalf("Connect: %v", err)
	}
	if calls := ctrl.snapshot(); len(calls) != 1 || calls[0] != "STATUS" {
		t.Errorf("wpactrl calls = %v, want [STATUS]", calls)
	}
	if calls := dhcp.snapshot(); len(calls) != 1 || calls[0] != "dhcp wlan0 "+testMAC().String() {
		t.Errorf("dhcp calls = %v", calls)
	}
	if calls := r.snapshot(); len(calls) != 0 {
		t.Errorf("unexpected external commands during Connect: %v", calls)
	}

	w.StopDHCP()
	if got := dhcp.stopCount(); got != 1 {
		t.Errorf("StopDHCP count = %d, want 1", got)
	}
}

func TestWifiConnectTimesOut(t *testing.T) {
	r := newFakeRunner(nil)
	ctrl := newFakeWPACtrl()
	ctrl.replies["STATUS"] = [][]byte{[]byte("wpa_state=SCANNING\n")}

	w := NewWifiClient("wlan0", testMAC(), r, newFakeOps(), mustWifiStore(t), testLogger())
	w.Ctrl = ctrl
	w.DHCP = newFakeDHCP(true).fn()
	w.PollInterval = time.Millisecond
	w.ConnectTimeout = time.Millisecond

	if err := w.Connect(context.Background()); err == nil {
		t.Fatal("expected a timeout error")
	}
}

func TestWifiScan(t *testing.T) {
	r := newFakeRunner(nil)
	ctrl := newFakeWPACtrl()
	ctrl.replies["SCAN"] = [][]byte{[]byte("OK\n")}
	ctrl.replies["SCAN_RESULTS"] = [][]byte{readTestdata(t, "scan_results.txt")}

	w := NewWifiClient("wlan0", testMAC(), r, newFakeOps(), mustWifiStore(t), testLogger())
	w.Ctrl = ctrl

	nets, err := w.Scan(context.Background())
	if err != nil {
		t.Fatalf("Scan: %v", err)
	}
	if len(nets) != 3 || nets[0].SSID != "home-wifi" {
		t.Fatalf("nets = %+v", nets)
	}
	calls := strings.Join(ctrl.snapshot(), ",")
	if calls != "SCAN,SCAN_RESULTS" {
		t.Errorf("scan sequence = %q, want SCAN,SCAN_RESULTS", calls)
	}
	if runCalls := r.snapshot(); len(runCalls) != 0 {
		t.Errorf("unexpected external commands during Scan: %v", runCalls)
	}
}

func TestWifiConnectAndScanOverUnixSocket(t *testing.T) {
	statusBody := readTestdata(t, "wpa_status.txt")
	scanBody := readTestdata(t, "scan_results.txt")
	ctrlDir := startFakeWPAServer(t, "wlan0", func(conn *net.UnixConn, peer *net.UnixAddr, cmd string) {
		switch cmd {
		case "STATUS":
			_, _ = conn.WriteToUnix(statusBody, peer)
		case "SCAN":
			_, _ = conn.WriteToUnix([]byte("OK\n"), peer)
		case "SCAN_RESULTS":
			_, _ = conn.WriteToUnix(scanBody, peer)
		default:
			t.Errorf("unexpected wpa command %q", cmd)
		}
	})

	r := newFakeRunner(nil)
	dhcp := newFakeDHCP(true)
	w := NewWifiClient("wlan0", testMAC(), r, newFakeOps(), mustWifiStore(t), testLogger())
	w.CtrlDir = ctrlDir // leave w.Ctrl nil to exercise default SocketWPACtrl
	w.DHCP = dhcp.fn()
	w.PollInterval = time.Millisecond
	w.ConnectTimeout = time.Second

	if err := w.Connect(context.Background()); err != nil {
		t.Fatalf("Connect: %v", err)
	}
	if got := w.CurrentSSID(context.Background()); got != "home-wifi" {
		t.Fatalf("CurrentSSID = %q, want home-wifi", got)
	}
	nets, err := w.Scan(context.Background())
	if err != nil {
		t.Fatalf("Scan: %v", err)
	}
	if len(nets) != 3 || nets[0].SSID != "home-wifi" {
		t.Fatalf("nets = %+v", nets)
	}
	if runCalls := r.snapshot(); len(runCalls) != 0 {
		t.Fatalf("unexpected external commands: %v", runCalls)
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
