package bot

import (
	"strings"
	"testing"
)

func TestStatusTextWarnsAboutUnsyncedClock(t *testing.T) {
	e := newEnv(t)
	e.bot.Status.SetClockSynced(false)
	if got := e.bot.statusText(); !strings.Contains(got, "not synced") {
		t.Fatalf("no clock warning:\n%s", got)
	}
	e.bot.Status.SetClockSynced(true)
	if got := e.bot.statusText(); strings.Contains(got, "not synced") {
		t.Fatalf("unexpected clock warning:\n%s", got)
	}
}

type fakeNetwork struct{ ifaces []NetIface }

func (f fakeNetwork) Interfaces() []NetIface { return f.ifaces }

func TestStatusTextListsInterfaces(t *testing.T) {
	e := newEnv(t)
	e.bot.Network = fakeNetwork{ifaces: []NetIface{
		{Name: "eth0", Kind: "ethernet", IPs: []string{"192.168.1.40"}},
		{Name: "wlan0", Kind: "wifi", SSID: "WA-Setup", APClients: 1},
		{Name: "wlan1", Kind: "wifi", SSID: "MyWifi", IPs: []string{"192.168.1.41"}},
	}}
	got := e.bot.statusText()
	want := "eth0 192.168.1.40, wlan0 AP WA-Setup, wlan1 MyWifi 192.168.1.41"
	if !strings.Contains(got, want) {
		t.Fatalf("interfaces line missing %q:\n%s", want, got)
	}
}

func TestStatusTextWithoutNetworkKeepsURLs(t *testing.T) {
	e := newEnv(t)
	got := e.bot.statusText()
	if strings.Contains(got, "Interfaces:") || !strings.Contains(got, "192.168.1.40") {
		t.Fatalf("unexpected status:\n%s", got)
	}
}
