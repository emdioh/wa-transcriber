package web

import (
	"context"
	"net/http/httptest"
	"net/url"
	"strings"
	"testing"
	"time"

	"github.com/emdioh/wa-transcriber/internal/netmgr"
)

type fakeNet struct {
	interfaces []netmgr.Interface
	nets       []netmgr.Network
	saved      []netmgr.SavedNetwork
	policy     netmgr.APPolicy
	apMode     bool
	scanErr    error
	saveErr    error
	savedCalls []string
	forgotten  []string
}

func (f *fakeNet) Start(context.Context) error { return nil }
func (f *fakeNet) Status() netmgr.Status       { return netmgr.Status{Interfaces: f.interfaces} }
func (f *fakeNet) Scan(context.Context) ([]netmgr.Network, error) {
	return f.nets, f.scanErr
}
func (f *fakeNet) SaveNetwork(ssid, pass string) error {
	if f.saveErr != nil {
		return f.saveErr
	}
	f.savedCalls = append(f.savedCalls, ssid+":"+pass)
	f.saved = append(f.saved, netmgr.SavedNetwork{SSID: ssid})
	return nil
}
func (f *fakeNet) ForgetNetwork(ssid string) error {
	f.forgotten = append(f.forgotten, ssid)
	return nil
}
func (f *fakeNet) SetAPPolicy(p netmgr.APPolicy) error  { f.policy = p; return nil }
func (f *fakeNet) APPolicy() netmgr.APPolicy            { return f.policy }
func (f *fakeNet) SavedNetworks() []netmgr.SavedNetwork { return f.saved }
func (f *fakeNet) InAPMode() bool                       { return f.apMode }
func (f *fakeNet) Erase() error                         { return nil }

func newNetHarness(t *testing.T) (*harness, *fakeNet) {
	t.Helper()
	h := newHarness(t)
	h.srv.Close()
	nm := &fakeNet{policy: netmgr.APAuto}
	s := New(Options{
		Config: h.cfg, Status: h.st, Logs: h.logs, WA: h.wa,
		Getenv:  func(k string) string { return h.env[k] },
		Network: nm, NetworkManaged: true,
		Version: "test",
	})
	h.srv = httptest.NewServer(s.Handler())
	t.Cleanup(h.srv.Close)
	h.c = h.newClient()
	h.setup()
	return h, nm
}

func TestNetworkPageShowsInterfacesAndSaved(t *testing.T) {
	h, nm := newNetHarness(t)
	nm.interfaces = []netmgr.Interface{
		{Name: "eth0", Kind: netmgr.KindEthernet, State: netmgr.StateUp, IPs: []string{"10.0.0.2"}},
		{Name: "wlan0", Kind: netmgr.KindWiFi, State: netmgr.StateUp, SSID: "home"},
	}
	nm.saved = []netmgr.SavedNetwork{{SSID: "home", LastOK: time.Now()}}
	_, _, body := h.get(h.c, "/network")
	for _, want := range []string{"wlan0", "home", "Forget", "Scan for networks"} {
		if !strings.Contains(body, want) {
			t.Errorf("page missing %q", want)
		}
	}
}

func TestNetworkScanStoresResults(t *testing.T) {
	h, nm := newNetHarness(t)
	nm.nets = []netmgr.Network{{SSID: "office", Signal: -50, Secured: true}}
	_, _, body := h.post(h.c, "/network/scan", url.Values{})
	if !strings.Contains(body, "1 networks found") {
		t.Fatalf("scan flash missing: %s", body)
	}
	_, _, body = h.get(h.c, "/network")
	if !strings.Contains(body, "office") || !strings.Contains(body, "-50 dBm") {
		t.Fatalf("scan results not shown: %s", body)
	}
}

func TestNetworkConnectValidates(t *testing.T) {
	h, nm := newNetHarness(t)
	_, _, body := h.post(h.c, "/network/connect", url.Values{"ssid": {""}})
	if !strings.Contains(body, "Choose a network") {
		t.Fatalf("empty ssid accepted: %s", body)
	}
	_, _, body = h.post(h.c, "/network/connect", url.Values{"ssid": {"home"}, "passphrase": {"short"}, "secured": {"1"}})
	if !strings.Contains(body, "at least 8") {
		t.Fatalf("short passphrase accepted: %s", body)
	}
	if len(nm.savedCalls) != 0 {
		t.Fatalf("saved a bad network: %v", nm.savedCalls)
	}
	_, _, body = h.post(h.c, "/network/connect", url.Values{"ssid": {"home"}, "passphrase": {"longenough"}})
	if !strings.Contains(body, "Saved") {
		t.Fatalf("connect failed: %s", body)
	}
	if len(nm.savedCalls) != 1 || nm.savedCalls[0] != "home:longenough" {
		t.Fatalf("savedCalls = %v", nm.savedCalls)
	}
}

func TestNetworkConnectFromAPShowsSwitchPage(t *testing.T) {
	h, nm := newNetHarness(t)
	nm.apMode = true
	_, _, body := h.post(h.c, "/network/connect", url.Values{"ssid": {"home"}, "passphrase": {"longenough"}})
	if !strings.Contains(body, "Switching network") || !strings.Contains(body, "wa.local") {
		t.Fatalf("switch page missing: %s", body)
	}
}

func TestNetworkForgetAndPolicy(t *testing.T) {
	h, nm := newNetHarness(t)
	h.post(h.c, "/network/forget", url.Values{"ssid": {"home"}})
	if len(nm.forgotten) != 1 || nm.forgotten[0] != "home" {
		t.Fatalf("forgotten = %v", nm.forgotten)
	}
	_, _, body := h.post(h.c, "/network/policy", url.Values{"policy": {"always"}})
	if nm.policy != netmgr.APAlways || !strings.Contains(body, "always") {
		t.Fatalf("policy = %v, body: %s", nm.policy, body)
	}
}

// The WiFi card is the config section for the connection: it names the
// adapter, offers the scan and takes an SSID by hand, which is the only way
// to join a hidden network.
func TestNetworkPageHasWiFiSection(t *testing.T) {
	h, nm := newNetHarness(t)
	nm.interfaces = []netmgr.Interface{
		{Name: "eth0", Kind: netmgr.KindEthernet, State: netmgr.StateUp},
		{Name: "wlan0", Kind: netmgr.KindWiFi, State: netmgr.StateDown},
	}
	_, _, body := h.get(h.c, "/network")
	for _, want := range []string{
		"Adapter: <code>wlan0</code>",
		"Scan for networks",
		"Join a hidden network",
		"Network name (SSID)",
		"Setup access point",
	} {
		if !strings.Contains(body, want) {
			t.Errorf("page missing %q", want)
		}
	}
}

// Without an adapter there is nothing to scan and no setup AP to run: say so
// instead of offering a button that can only fail.
func TestNetworkPageWithoutAdapter(t *testing.T) {
	h, nm := newNetHarness(t)
	nm.interfaces = []netmgr.Interface{
		{Name: "eth0", Kind: netmgr.KindEthernet, State: netmgr.StateUp, IPs: []string{"10.0.0.2"}},
	}
	_, _, body := h.get(h.c, "/network")
	if !strings.Contains(body, "No WiFi adapter is connected") {
		t.Errorf("no-adapter hint missing: %s", body)
	}
	if strings.Contains(body, "Scan for networks") {
		t.Errorf("scan offered without an adapter: %s", body)
	}
	if strings.Contains(body, "Setup access point") {
		t.Errorf("setup AP policy offered without an adapter: %s", body)
	}
}

// A hidden SSID never appears in a scan, so the manual form must save it.
func TestNetworkConnectHiddenSSID(t *testing.T) {
	h, nm := newNetHarness(t)
	nm.interfaces = []netmgr.Interface{{Name: "wlan0", Kind: netmgr.KindWiFi, State: netmgr.StateUp}}
	_, _, body := h.post(h.c, "/network/connect", url.Values{
		"ssid": {"  CasaNascosta "}, "passphrase": {"longenough"}, "secured": {"1"},
	})
	if !strings.Contains(body, "Saved") {
		t.Fatalf("hidden network not saved: %s", body)
	}
	if len(nm.savedCalls) != 1 || nm.savedCalls[0] != "CasaNascosta:longenough" {
		t.Fatalf("savedCalls = %v", nm.savedCalls)
	}
}

func TestNetworkPageWithoutManager(t *testing.T) {
	h := newHarness(t)
	h.setup()
	_, _, body := h.get(h.c, "/network")
	if !strings.Contains(body, "managed by the host") {
		t.Fatalf("mode-none message missing: %s", body)
	}
}
