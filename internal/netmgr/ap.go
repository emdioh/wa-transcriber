package netmgr

import (
	"context"
	"fmt"
	"log/slog"
	"net"
	"os"
	"path/filepath"
	"strings"
	"sync"

	"github.com/emdioh/wa-transcriber/internal/netmgr/apsrv"
	"github.com/emdioh/wa-transcriber/internal/netmgr/netops"
)

// APServerStart launches the in-process setup-AP DHCPv4 + wildcard DNS server
// and returns a close function that stops it.
type APServerStart func(ctx context.Context, cfg apsrv.Config) (func() error, error)

func defaultAPServerStart(ctx context.Context, cfg apsrv.Config) (func() error, error) {
	srv, err := apsrv.Start(ctx, cfg)
	if err != nil {
		return nil, err
	}
	return srv.Close, nil
}

// AP manages the setup access point: a WPA2 network served by hostapd with
// in-process DHCP and wildcard DNS from apsrv, so a phone can always reach the
// web UI.
type AP struct {
	IfName      string
	MAC         net.HardwareAddr
	Runner      Runner
	Ops         netops.Ops
	StartServer APServerStart
	Log         *slog.Logger

	// WatTxtPath is the boot-partition settings file (FAT, editable from any
	// PC). M6 creates it on the images.
	WatTxtPath      string
	HostapdConfPath string
	IP              string
	PrefixLen       int

	mu   sync.Mutex
	stop func()
}

// NewAP returns a configured access-point controller. If ops is nil,
// netops.Default() is used.
func NewAP(ifname string, mac net.HardwareAddr, run Runner, ops netops.Ops, log *slog.Logger) *AP {
	if log == nil {
		log = slog.Default()
	}
	if ops == nil {
		ops = netops.Default()
	}
	return &AP{
		IfName:          ifname,
		MAC:             mac,
		Runner:          run,
		Ops:             ops,
		StartServer:     defaultAPServerStart,
		Log:             log,
		WatTxtPath:      "/boot/wat.txt",
		HostapdConfPath: "/run/wat/hostapd.conf",
		IP:              "192.168.4.1",
		PrefixLen:       24,
	}
}

func (a *AP) ops() netops.Ops {
	if a.Ops != nil {
		return a.Ops
	}
	return netops.Default()
}

func (a *AP) startServer() APServerStart {
	if a.StartServer != nil {
		return a.StartServer
	}
	return defaultAPServerStart
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

// Start brings the interface up as the setup AP and launches hostapd and the
// in-process DHCP/DNS server. It stops any previously started instance first.
func (a *AP) Start(ctx context.Context) error {
	serverIP := net.ParseIP(a.IP).To4()
	if serverIP == nil {
		return fmt.Errorf("ap: invalid IPv4 address %q", a.IP)
	}
	ops := a.ops()
	if err := ops.AddrFlush(a.IfName); err != nil {
		return err
	}
	if err := ops.AddrAdd(a.IfName, serverIP, a.PrefixLen); err != nil {
		return err
	}
	if err := ops.LinkUp(a.IfName); err != nil {
		return err
	}
	if err := a.WriteHostapdConf(); err != nil {
		return err
	}

	stopH, _, err := a.Runner.Start(ctx, "hostapd", a.HostapdConfPath)
	if err != nil {
		return err
	}
	closeSrv, err := a.startServer()(ctx, apsrv.Config{
		IfName:   a.IfName,
		ServerIP: serverIP,
		Mask:     net.CIDRMask(a.PrefixLen, 32),
		Log:      a.Log,
	})
	if err != nil {
		if stopH != nil {
			stopH()
		}
		return err
	}

	stop := func() {
		if closeSrv != nil {
			_ = closeSrv()
		}
		if stopH != nil {
			stopH()
		}
	}
	a.mu.Lock()
	prev := a.stop
	a.stop = stop
	a.mu.Unlock()
	if prev != nil {
		prev()
	}
	return nil
}

// Stop terminates the DHCP/DNS server and hostapd, and flushes the interface
// addresses.
func (a *AP) Stop(_ context.Context) error {
	a.mu.Lock()
	stop := a.stop
	a.stop = nil
	a.mu.Unlock()
	if stop != nil {
		stop()
	}
	return a.ops().AddrFlush(a.IfName)
}

// Stations reports how many clients are associated with the AP.
func (a *AP) Stations(_ context.Context) (int, error) {
	return a.ops().StationCount(a.IfName)
}

// watValue reads key=value lines from wat.txt (shared parser, see
// provision.go for the Windows-line-ending and comment handling).
func (a *AP) watValue(want string) string {
	data, err := os.ReadFile(a.WatTxtPath)
	if err != nil {
		return ""
	}
	return watValue(data, want)
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
