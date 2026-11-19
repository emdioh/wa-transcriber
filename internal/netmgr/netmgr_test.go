package netmgr

import (
	"context"
	"strings"
	"testing"

	"github.com/emdioh/wa-transcriber/internal/status"
)

func TestNewModes(t *testing.T) {
	for _, mode := range []string{"", "bogus", "NONE"} {
		if _, err := New(mode); err == nil {
			t.Errorf("New(%q) accepted an invalid mode", mode)
		}
	}
	if _, err := New("managed"); err == nil {
		t.Fatal("New(managed) must fail until the managed implementation exists")
	}
	m, err := New("none")
	if err != nil {
		t.Fatalf("New(none): %v", err)
	}
	if err := m.Start(context.Background()); err != nil {
		t.Fatalf("none.Start: %v", err)
	}
}

func TestNoneMutationsAreDisabled(t *testing.T) {
	m, _ := New("none")
	if _, err := m.Scan(context.Background()); err == nil {
		t.Error("Scan must fail in mode none")
	}
	if err := m.SaveNetwork("home", "secret"); err == nil {
		t.Error("SaveNetwork must fail in mode none")
	}
	if err := m.ForgetNetwork("home"); err == nil {
		t.Error("ForgetNetwork must fail in mode none")
	}
	if err := m.SetAPPolicy(APAlways); err == nil {
		t.Error("SetAPPolicy must fail in mode none")
	}
	if err := m.Erase(); err != nil {
		t.Errorf("Erase must be a no-op in mode none, got %v", err)
	}
}

func TestNoneStatusGroupsAddresses(t *testing.T) {
	n := &none{addrs: func() []status.Addr {
		return []status.Addr{
			{Interface: "wlan0", IP: "192.168.1.9"},
			{Interface: "eth0", IP: "10.0.0.2"},
			{Interface: "eth0", IP: "fe80::1"},
			{Interface: "eth0", IP: "192.168.0.3"},
			{Interface: "docker0", IP: "172.17.0.1"},
		}
	}}
	st := n.Status()
	if len(st.Interfaces) != 3 {
		t.Fatalf("interfaces = %d, want 3", len(st.Interfaces))
	}
	got := map[string]Interface{}
	for _, i := range st.Interfaces {
		got[i.Name] = i
	}
	if got["eth0"].Kind != KindEthernet || got["wlan0"].Kind != KindWiFi || got["docker0"].Kind != KindOther {
		t.Errorf("kinds = %v %v %v", got["eth0"].Kind, got["wlan0"].Kind, got["docker0"].Kind)
	}
	if got["eth0"].State != StateUp {
		t.Errorf("eth0 state = %q", got["eth0"].State)
	}
	if len(got["eth0"].IPs) != 3 {
		t.Errorf("eth0 IPs = %v, want 3", got["eth0"].IPs)
	}
	if got["eth0"].SSID != "" || got["eth0"].APClients != 0 {
		t.Errorf("eth0 has WiFi fields set: %+v", got["eth0"])
	}
}

func TestStatusFromAddrsEmptyAndSorted(t *testing.T) {
	if got := StatusFromAddrs(nil); len(got.Interfaces) != 0 {
		t.Fatalf("nil addrs -> %+v", got)
	}
	st := StatusFromAddrs([]status.Addr{
		{Interface: "zeth0", IP: "10.0.0.1"},
		{Interface: "aeth0", IP: "10.0.0.2"},
	})
	if len(st.Interfaces) != 2 || st.Interfaces[0].Name != "aeth0" || st.Interfaces[1].Name != "zeth0" {
		t.Fatalf("not sorted: %+v", st.Interfaces)
	}
	if strings.Join(st.Interfaces[0].IPs, ",") != "10.0.0.2" {
		t.Fatalf("IPs not grouped: %+v", st.Interfaces[0])
	}
}
