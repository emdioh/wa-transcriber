package netmgr

import (
	"context"
	"encoding/hex"
	"fmt"
	"log/slog"
	"os"
	"path/filepath"
	"sort"
	"strconv"
	"strings"
	"time"
)

// WifiClient manages one WiFi interface in client mode with wpa_supplicant.
type WifiClient struct {
	IfName string
	Runner Runner
	Store  *WifiStore
	// CtrlDir is the wpa_supplicant control directory (/run/wat/wpa).
	CtrlDir string
	// ConfPath is the generated wpa_supplicant config (/run/wat/wpa.conf).
	ConfPath string
	Log      *slog.Logger

	PollInterval   time.Duration // wpa_cli status polling interval
	ConnectTimeout time.Duration // time to wait for wpa_state=COMPLETED
	RestartDelay   time.Duration // delay before restarting wpa_supplicant
}

// NewWifiClient returns a configured WiFi client.
func NewWifiClient(ifname string, run Runner, store *WifiStore, log *slog.Logger) *WifiClient {
	if log == nil {
		log = slog.Default()
	}
	return &WifiClient{
		IfName:         ifname,
		Runner:         run,
		Store:          store,
		CtrlDir:        "/run/wat/wpa",
		ConfPath:       "/run/wat/wpa.conf",
		Log:            log,
		PollInterval:   500 * time.Millisecond,
		ConnectTimeout: 45 * time.Second,
		RestartDelay:   2 * time.Second,
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

// Connect waits until wpa_cli reports wpa_state=COMPLETED and then runs
// DHCP. It returns once the interface has a lease or the timeout elapses.
func (w *WifiClient) Connect(ctx context.Context) error {
	deadline := time.Now().Add(w.ConnectTimeout)
	for time.Now().Before(deadline) {
		out, err := w.Runner.Run(ctx, "wpa_cli", "-p", w.CtrlDir, "-i", w.IfName, "status")
		if err == nil {
			if state, _ := parseWPACliStatus(out); state == "COMPLETED" {
				if dhcpLease(ctx, w.Runner, w.IfName, 15*time.Second) {
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

// Scan asks wpa_supplicant for visible networks and returns them deduped by
// SSID, keeping the strongest signal.
func (w *WifiClient) Scan(ctx context.Context) ([]Network, error) {
	if _, err := w.Runner.Run(ctx, "wpa_cli", "-p", w.CtrlDir, "-i", w.IfName, "scan"); err != nil {
		return nil, err
	}
	out, err := w.Runner.Run(ctx, "wpa_cli", "-p", w.CtrlDir, "-i", w.IfName, "scan_results")
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

// parseWPACliStatus extracts wpa_state and ssid from wpa_cli status output.
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

// parseScanResults parses wpa_cli scan_results output (tab-separated:
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
