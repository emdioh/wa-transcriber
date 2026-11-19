package netmgr

import (
	"context"
	"fmt"
	"log/slog"
	"net"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"sync"
)

// AP manages the setup access point: a WPA2 network served by hostapd with
// DHCP and DNS from dnsmasq, so a phone can always reach the web UI.
type AP struct {
	IfName string
	MAC    net.HardwareAddr
	Runner Runner
	Log    *slog.Logger

	// WatTxtPath is the boot-partition settings file (FAT, editable from any
	// PC). M6 creates it on the images.
	WatTxtPath      string
	HostapdConfPath string
	IP              string
	PrefixLen       int

	mu   sync.Mutex
	stop func()
}

// NewAP returns a configured access-point controller.
func NewAP(ifname string, mac net.HardwareAddr, run Runner, log *slog.Logger) *AP {
	if log == nil {
		log = slog.Default()
	}
	return &AP{
		IfName:          ifname,
		MAC:             mac,
		Runner:          run,
		Log:             log,
		WatTxtPath:      "/boot/wat.txt",
		HostapdConfPath: "/run/wat/hostapd.conf",
		IP:              "192.168.4.1",
		PrefixLen:       24,
	}
}

// SetupKey returns the WPA2 passphrase for the setup AP: the value of
// setup_ap_key in wat.txt, or a fallback derived from the MAC address.
func (a *AP) SetupKey() string {
	if key := a.watValue("setup_ap_key"); len(key) >= 8 {
		return key
	}
	key := "wa-setup-" + lastN(macHex(a.MAC), 6)
	a.Log.Warn("setup AP key missing or too short, using the fallback key",
		"fallback", key, "wat_txt", a.WatTxtPath)
	return key
}

// CountryCode returns the ISO country code for hostapd, default IT.
func (a *AP) CountryCode() string {
	if c := a.watValue("country"); c != "" {
		return c
	}
	return "IT"
}

// WriteHostapdConf writes the hostapd config and returns its path.
func (a *AP) WriteHostapdConf() error {
	if err := os.MkdirAll(filepath.Dir(a.HostapdConfPath), 0o700); err != nil {
		return err
	}
	conf := renderHostapdConf(a.IfName, "WA-Setup-"+lastN(macHex(a.MAC), 4), a.CountryCode(), a.SetupKey())
	return os.WriteFile(a.HostapdConfPath, []byte(conf), 0o600)
}

// Start brings the interface up as the setup AP and launches hostapd and
// dnsmasq. It stops any previously started instance first.
func (a *AP) Start(ctx context.Context) error {
	if _, err := a.Runner.Run(ctx, "ip", "addr", "flush", "dev", a.IfName); err != nil {
		return err
	}
	if _, err := a.Runner.Run(ctx, "ip", "addr", "add", a.IP+"/"+strconv.Itoa(a.PrefixLen), "dev", a.IfName); err != nil {
		return err
	}
	if _, err := a.Runner.Run(ctx, "ip", "link", "set", a.IfName, "up"); err != nil {
		return err
	}
	if err := a.WriteHostapdConf(); err != nil {
		return err
	}

	stopH, _, err := a.Runner.Start(ctx, "hostapd", a.HostapdConfPath)
	if err != nil {
		return err
	}
	stopD, _, err := a.Runner.Start(ctx, "dnsmasq", a.dnsmasqArgs()...)
	if err != nil {
		stopH()
		return err
	}

	stop := func() { stopD(); stopH() }
	a.mu.Lock()
	if a.stop != nil {
		a.stop()
	}
	a.stop = stop
	a.mu.Unlock()
	return nil
}

// Stop terminates hostapd/dnsmasq and flushes the interface addresses.
func (a *AP) Stop(ctx context.Context) error {
	a.mu.Lock()
	stop := a.stop
	a.stop = nil
	a.mu.Unlock()
	if stop != nil {
		stop()
	}
	_, err := a.Runner.Run(ctx, "ip", "addr", "flush", "dev", a.IfName)
	return err
}

// Stations reports how many clients are associated with the AP.
func (a *AP) Stations(ctx context.Context) (int, error) {
	out, err := a.Runner.Run(ctx, "iw", "dev", a.IfName, "station", "dump")
	if err != nil {
		return 0, err
	}
	count := 0
	for _, line := range strings.Split(string(out), "\n") {
		if strings.HasPrefix(strings.TrimSpace(line), "Station ") {
			count++
		}
	}
	return count, nil
}

func (a *AP) dnsmasqArgs() []string {
	return []string{
		"--keep-in-foreground",
		"--no-resolv",
		"--no-hosts",
		"--interface=" + a.IfName,
		"--bind-interfaces",
		"--dhcp-range=192.168.4.10,192.168.4.100,255.255.255.0,1h",
		"--address=/#/" + a.IP,
		"--dhcp-option=114,http://" + a.IP + "/",
	}
}

// watValue reads key=value lines from wat.txt.
func (a *AP) watValue(want string) string {
	data, err := os.ReadFile(a.WatTxtPath)
	if err != nil {
		return ""
	}
	for _, line := range strings.Split(string(data), "\n") {
		key, val, ok := strings.Cut(strings.TrimSpace(line), "=")
		if ok && key == want {
			return strings.TrimSpace(val)
		}
	}
	return ""
}

func renderHostapdConf(ifname, ssid, country, key string) string {
	return fmt.Sprintf("interface=%s\n"+
		"driver=nl80211\n"+
		"ssid=%s\n"+
		"hw_mode=g\n"+
		"channel=6\n"+
		"country_code=%s\n"+
		"ieee80211d=1\n"+
		"wpa=2\n"+
		"wpa_key_mgmt=WPA-PSK\n"+
		"rsn_pairwise=CCMP\n"+
		"wpa_passphrase=%s\n", ifname, ssid, country, key)
}

func macHex(mac net.HardwareAddr) string {
	return strings.ToUpper(strings.ReplaceAll(mac.String(), ":", ""))
}

func lastN(s string, n int) string {
	if len(s) <= n {
		return s
	}
	return s[len(s)-n:]
}
