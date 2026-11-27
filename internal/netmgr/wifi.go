package netmgr

import (
	"context"
	"encoding/hex"
	"fmt"
	"log/slog"
	"net"
	"os"
	"path/filepath"
	"sort"
	"strconv"
	"strings"
	"sync"
	"time"

	"github.com/emdioh/wa-transcriber/internal/netmgr/dhcpc"
	"github.com/emdioh/wa-transcriber/internal/netmgr/netops"
)

// WifiClient manages one WiFi interface in client mode with wpa_supplicant.
type WifiClient struct {
	IfName string
	MAC    net.HardwareAddr
	Runner Runner
	Ops    netops.Ops
	Ctrl   WPACtrl
	DHCP   DHCPFunc
	Store  *WifiStore
	// CtrlDir is the wpa_supplicant control directory (/run/wat/wpa).
	CtrlDir string
	// ConfPath is the generated wpa_supplicant config (/run/wat/wpa.conf).
	ConfPath string
	Log      *slog.Logger

	PollInterval   time.Duration // control socket status polling interval
	ConnectTimeout time.Duration // time to wait for wpa_state=COMPLETED
	DHCPTimeout    time.Duration // deadline for one DHCP attempt after COMPLETED
	RestartDelay   time.Duration // delay before restarting wpa_supplicant

	mu       sync.Mutex
	stopDHCP func()
}

// NewWifiClient returns a configured WiFi client. If ops is nil,
// netops.Default() is used.
func NewWifiClient(ifname string, mac net.HardwareAddr, run Runner, ops netops.Ops, store *WifiStore, log *slog.Logger) *WifiClient {
	if log == nil {
		log = slog.Default()
	}
	if ops == nil {
		ops = netops.Default()
	}
	return &WifiClient{
		IfName:         ifname,
		MAC:            mac,
		Runner:         run,
		Ops:            ops,
		DHCP:           defaultDHCPFunc(ops, dhcpc.DefaultResolvConf, log),
		Store:          store,
		CtrlDir:        "/run/wat/wpa",
		ConfPath:       "/run/wat/wpa.conf",
		Log:            log,
		PollInterval:   500 * time.Millisecond,
		ConnectTimeout: 45 * time.Second,
		DHCPTimeout:    15 * time.Second,
		RestartDelay:   2 * time.Second,
	}
}

func (w *WifiClient) ops() netops.Ops {
	if w.Ops != nil {
		return w.Ops
	}
	return netops.Default()
}

func (w *WifiClient) ctrl() WPACtrl {
	if w.Ctrl != nil {
		return w.Ctrl
	}
	return &SocketWPACtrl{CtrlDir: w.CtrlDir, IfName: w.IfName}
}

func (w *WifiClient) dhcpFn() DHCPFunc {
	if w.DHCP != nil {
		return w.DHCP
	}
	return defaultDHCPFunc(w.ops(), dhcpc.DefaultResolvConf, w.Log)
}

func (w *WifiClient) dhcpTimeout() time.Duration {
	if w.DHCPTimeout > 0 {
		return w.DHCPTimeout
	}
	return 15 * time.Second
}

// StopDHCP cancels any background DHCP lease renewal loop without dropping the
// interface's current address (D16).
func (w *WifiClient) StopDHCP() {
	w.mu.Lock()
	stop := w.stopDHCP
	w.stopDHCP = nil
	w.mu.Unlock()
	if stop != nil {
		stop()
	}
}

func (w *WifiClient) setStopDHCP(ctx context.Context, stop func()) {
	if stop == nil {
		return
	}
	if ctx.Err() != nil {
		stop()
		return
	}
	w.mu.Lock()
	prev := w.stopDHCP
	w.stopDHCP = stop
	w.mu.Unlock()
	if prev != nil {
		prev()
	}
}

// WriteConf writes the wpa_supplicant config with every saved network. SSIDs
// use the ssid=<hex> form, so names with spaces, quotes or non-ASCII bytes
// never need escaping.
func (w *WifiClient) WriteConf() error {
	if err := os.MkdirAll(filepath.Dir(w.ConfPath), 0o700); err != nil {
		return err
	}
	if err := os.MkdirAll(w.CtrlDir, 0o700); err != nil {
		return err
	}
	return os.WriteFile(w.ConfPath, []byte(renderWPAConf(w.CtrlDir, w.Store.List())), 0o600)
}

// Start launches wpa_supplicant. The caller supervises the process (see
// Supervise).
func (w *WifiClient) Start(ctx context.Context) (func(), func() error, error) {
	return w.Runner.Start(ctx, "wpa_supplicant", "-i", w.IfName, "-D", "nl80211", "-c", w.ConfPath)
}

// Supervise keeps wpa_supplicant running until ctx ends, restarting it when
// it exits.
func (w *WifiClient) Supervise(ctx context.Context) {
	for ctx.Err() == nil {
		_, wait, err := w.Start(ctx)
		if err != nil {
			w.Log.Error("wifi: start wpa_supplicant", "iface", w.IfName, "err", err)
			if !sleepCtx(ctx, w.RestartDelay) {
				return
			}
			continue
		}
		err = wait()
		if ctx.Err() != nil {
			return
		}
		w.Log.Warn("wifi: wpa_supplicant exited", "iface", w.IfName, "err", err)
		if !sleepCtx(ctx, w.RestartDelay) {
			return
		}
	}
}

// Connect waits until wpa_supplicant reports wpa_state=COMPLETED over its
// control socket and then runs DHCP. It returns once the interface has a lease
// or the timeout elapses.
func (w *WifiClient) Connect(ctx context.Context) error {
	deadline := time.Now().Add(w.ConnectTimeout)
	for time.Now().Before(deadline) {
		out, err := w.ctrl().Command(ctx, "STATUS")
		if err == nil {
			if state, _ := parseWPACliStatus(out); state == "COMPLETED" {
				// Lease application uses AddrReplacePrimary (D16): it keeps an
				// unchanged address (or replaces it if the lease changed)
				// without tearing the link down.
				if ok, stop := w.dhcpFn()(ctx, w.IfName, w.MAC, w.dhcpTimeout()); ok {
					w.setStopDHCP(ctx, stop)
					return nil
				}
			}
		}
		if !sleepCtx(ctx, w.PollInterval) {
			return ctx.Err()
		}
	}
	return fmt.Errorf("wifi: %s did not reach COMPLETED within %s", w.IfName, w.ConnectTimeout)
}

// CurrentSSID queries wpa_supplicant over its control socket for the active
// SSID. Returns "" on error or if not associated.
func (w *WifiClient) CurrentSSID(ctx context.Context) string {
	out, err := w.ctrl().Command(ctx, "STATUS")
	if err != nil {
		return ""
	}
	_, ssid := parseWPACliStatus(out)
	return ssid
}

// Scan asks wpa_supplicant for visible networks and returns them deduped by
// SSID, keeping the strongest signal.
func (w *WifiClient) Scan(ctx context.Context) ([]Network, error) {
	if _, err := w.ctrl().Command(ctx, "SCAN"); err != nil {
		return nil, err
	}
	out, err := w.ctrl().Command(ctx, "SCAN_RESULTS")
	if err != nil {
		return nil, err
	}
	return parseScanResults(out), nil
}

// renderWPAConf builds the config text for the given saved networks.
func renderWPAConf(ctrlDir string, networks []SavedNetwork) string {
	var b strings.Builder
	fmt.Fprintf(&b, "ctrl_interface=%s\n", ctrlDir)
	b.WriteString("update_config=0\n")
	for _, n := range networks {
		fmt.Fprintf(&b, "network={\n\tssid=%s\n\tpsk=%s\n\tpriority=%d\n}\n",
			hex.EncodeToString([]byte(n.SSID)), n.PSK, n.Priority)
	}
	return b.String()
}

// parseWPACliStatus extracts wpa_state and ssid from wpa_supplicant STATUS output.
func parseWPACliStatus(out []byte) (state, ssid string) {
	for _, line := range strings.Split(string(out), "\n") {
		key, val, ok := strings.Cut(line, "=")
		if !ok {
			continue
		}
		switch key {
		case "wpa_state":
			state = val
		case "ssid":
			ssid = val
		}
	}
	return state, ssid
}

// parseScanResults parses wpa_supplicant SCAN_RESULTS output (tab-separated:
// bssid, frequency, signal level, flags, ssid).
func parseScanResults(out []byte) []Network {
	var nets []Network
	for _, line := range strings.Split(string(out), "\n") {
		fields := strings.Split(line, "\t")
		if len(fields) < 5 || fields[4] == "" {
			continue
		}
		if strings.Contains(fields[0], "bssid") { // header line
			continue
		}
		sig, err := strconv.Atoi(strings.TrimSpace(fields[2]))
		if err != nil {
			continue
		}
		flags := fields[3]
		nets = append(nets, Network{
			SSID:    fields[4],
			Signal:  sig,
			Secured: strings.Contains(flags, "WPA") || strings.Contains(flags, "WEP"),
		})
	}
	return dedupeNetworks(nets)
}

func dedupeNetworks(nets []Network) []Network {
	best := map[string]Network{}
	for _, n := range nets {
		if cur, ok := best[n.SSID]; !ok || n.Signal > cur.Signal {
			best[n.SSID] = n
		}
	}
	out := make([]Network, 0, len(best))
	for _, n := range best {
		out = append(out, n)
	}
	sort.Slice(out, func(i, j int) bool { return out[i].Signal > out[j].Signal })
	return out
}

func sleepCtx(ctx context.Context, d time.Duration) bool {
	t := time.NewTimer(d)
	defer t.Stop()
	select {
	case <-ctx.Done():
		return false
	case <-t.C:
		return true
	}
}
