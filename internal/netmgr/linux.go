package netmgr

import (
	"context"
	"fmt"
	"log/slog"
	"net"
	"sync"
	"time"

	"github.com/emdioh/wa-transcriber/internal/netmgr/mdns"
	"github.com/emdioh/wa-transcriber/internal/status"
)

// managed is mode "managed": it discovers the interfaces, drives ethernet,
// the WiFi client and the setup AP, and runs the policy loop from policy.go.
type managed struct {
	fsys FS
	base string
	run  Runner
	log  *slog.Logger
	// store holds the saved WiFi networks (nil: none persisted).
	store *WifiStore
	// watTxtPath is the boot file the WiFi details can be provisioned from.
	watTxtPath string
	addrs      func() []status.Addr
	// paired tells whether WhatsApp is linked (drives the AP policy).
	paired func() bool

	pollInterval time.Duration
	timer        func(time.Duration) <-chan time.Time
	now          func() time.Time

	mu       sync.Mutex
	policy   APPolicy
	eths     map[string]*ethUnit
	wifis    map[string]*wifiUnit
	scanNets []Network
	scanAt   time.Time
}

type ethUnit struct {
	dev     *Ethernet
	carrier bool
	// known is false until the first poll has seen this interface. The
	// carrier file of an administratively down interface reads 0, and
	// nothing else brings the link up on a stock image (Buildroot's
	// S40network is not installed), so treating the zero carrier value as
	// "already down" would deadlock: Up() would never run, the link would
	// never come up and DHCP would never be attempted.
	known    bool
	busy     bool
	lastSeen time.Time
}

type wifiUnit struct {
	ifname  string
	mac     net.HardwareAddr
	client  *WifiClient
	ap      *AP
	state   wifiState
	reached time.Time // when the current state began
}

// NewManaged returns the managed manager. It needs root (it changes
// interfaces and binds ports 53/67).
func NewManaged(dataDir string, log *slog.Logger) (Manager, error) {
	store, err := OpenWifiStore(dataDir + "/wifi.json")
	if err != nil {
		return nil, err
	}
	return newManaged(store, status.Addresses, func() bool { return true }, log), nil
}

// newManaged is the constructor used by tests (fully injectable).
func newManaged(store *WifiStore, addrs func() []status.Addr, paired func() bool, log *slog.Logger) *managed {
	if log == nil {
		log = slog.Default()
	}
	return &managed{
		fsys:         osFS{},
		base:         "/sys/class/net",
		run:          execRunner{},
		log:          log,
		store:        store,
		watTxtPath:   WatTxtPath,
		addrs:        addrs,
		paired:       paired,
		policy:       APAuto,
		eths:         map[string]*ethUnit{},
		wifis:        map[string]*wifiUnit{},
		pollInterval: 2 * time.Second,
		timer:        time.After,
		now:          time.Now,
	}
}

// Start discovers the interfaces and runs the policy loop until ctx ends.
func (m *managed) Start(ctx context.Context) error {
	if err := m.discover(); err != nil {
		return err
	}
	// The card can carry the WiFi details (wat.txt): import them before the
	// first policy tick, so a prepared card joins by itself. Failure is not
	// fatal: the device still comes up in the setup AP with the cable/UI.
	if ssid, err := ProvisionWifi(m.watTxtPath, m.store); err != nil {
		m.log.Warn("network manager: wifi from wat.txt ignored", "file", m.watTxtPath, "err", err)
	} else if ssid != "" {
		m.log.Info("network manager: wifi network from wat.txt saved", "ssid", ssid, "file", m.watTxtPath)
	}
	m.log.Info("network manager: interfaces", "ethernet", len(m.eths), "wifi", len(m.wifis))
	go m.loop(ctx)
	go m.serveMDNS(ctx)
	return nil
}

// serveMDNS answers wa.local on every interface as soon as it has an address.
func (m *managed) serveMDNS(ctx context.Context) {
	r := &mdns.Responder{Name: "wa"}
	ifaces := func() []mdns.Iface {
		byName := map[string]*mdns.Iface{}
		for _, a := range m.addrs() {
			ifc, err := net.InterfaceByName(a.Interface)
			if err != nil {
				continue
			}
			ip := net.ParseIP(a.IP).To4()
			if ip == nil {
				continue
			}
			byName[a.Interface] = &mdns.Iface{Index: ifc.Index, IP: ip, Name: a.Interface}
		}
		out := make([]mdns.Iface, 0, len(byName))
		for _, i := range byName {
			out = append(out, *i)
		}
		return out
	}
	for ctx.Err() == nil {
		if len(ifaces()) == 0 {
			if !sleepCtx(ctx, m.pollInterval) {
				return
			}
			continue
		}
		if err := mdns.Listen(ctx, r, ifaces, m.log); err != nil && ctx.Err() == nil {
			m.log.Error("network manager: mdns", "err", err)
			if !sleepCtx(ctx, m.pollInterval) {
				return
			}
		}
	}
}

// discover fills the interface maps from sysfs. It is separate from Start so
// tests can drive step() deterministically.
func (m *managed) discover() error {
	ifaces, err := Discover(m.fsys, m.base)
	if err != nil {
		return fmt.Errorf("discover interfaces: %w", err)
	}
	for _, info := range ifaces {
		mac, err := net.InterfaceByName(info.Name)
		var hw net.HardwareAddr
		if err == nil {
			hw = mac.HardwareAddr
		}
		switch info.Kind {
		case KindEthernet:
			m.mu.Lock()
			m.eths[info.Name] = &ethUnit{dev: NewEthernet(info.Name, hw, m.run, m.log)}
			m.mu.Unlock()
		case KindWiFi:
			m.mu.Lock()
			m.wifis[info.Name] = &wifiUnit{
				ifname: info.Name,
				mac:    hw,
				client: NewWifiClient(info.Name, m.run, m.store, m.log),
				ap:     NewAP(info.Name, hw, m.run, m.log),
				state:  stateOff,
			}
			m.mu.Unlock()
		}
	}
	return nil
}

func (m *managed) loop(ctx context.Context) {
	for {
		t := m.timer(m.pollInterval)
		select {
		case <-ctx.Done():
			return
		case <-t:
			m.step(ctx)
		}
	}
}

func (m *managed) step(ctx context.Context) {
	ifaces, err := Discover(m.fsys, m.base)
	if err != nil {
		m.log.Error("network manager: discover", "err", err)
		return
	}
	for _, info := range ifaces {
		switch info.Kind {
		case KindEthernet:
			m.stepEthernet(ctx, info)
		case KindWiFi:
			m.stepWiFi(ctx, info)
		}
	}
}

// stepEthernet reacts to carrier changes. Ethernet.Up blocks (DHCP), so it
// runs in its own goroutine.
func (m *managed) stepEthernet(ctx context.Context, info InterfaceInfo) {
	m.mu.Lock()
	u := m.eths[info.Name]
	if u == nil {
		m.mu.Unlock()
		return
	}
	// The first poll after discovery always acts, even when the carrier
	// reads 0: that is the state of a link nobody has brought up yet.
	first := !u.known
	if (u.known && u.carrier == info.Carrier) || u.busy {
		m.mu.Unlock()
		return
	}
	u.known = true
	u.carrier = info.Carrier
	u.busy = true
	dev := u.dev
	m.mu.Unlock()

	go func(carrier bool) {
		defer func() {
			m.mu.Lock()
			u.busy = false
			m.mu.Unlock()
		}()
		// "up" also covers the first poll of a link that has never been
		// brought up: Ethernet.Up lifts the interface, which is what makes
		// carrier appear on drivers that report 0 while the link is down
		// (cpsw on the BeagleBone Black).
		if carrier || first {
			m.log.Info("ethernet: carrier up", "iface", info.Name, "first", first)
			dev.Up(ctx)
			return
		}
		m.log.Info("ethernet: carrier down", "iface", info.Name)
		if err := dev.Down(ctx); err != nil {
			m.log.Error("ethernet: down", "iface", info.Name, "err", err)
		}
	}(info.Carrier)
}

// stepWiFi evaluates the policy for one WiFi interface and runs its action.
func (m *managed) stepWiFi(ctx context.Context, info InterfaceInfo) {
	m.mu.Lock()
	u := m.wifis[info.Name]
	policy := m.policy
	m.mu.Unlock()
	if u == nil {
		return
	}

	in := inputs{
		Paired:      m.paired(),
		EthernetUp:  m.ethernetUp(),
		Policy:      policy,
		APAvailable: true,
	}
	if m.store != nil {
		in.Saved = len(m.store.List())
	}
	in.SavedVisible = m.savedVisible()
	in.ApClients = u.ap.stations()
	in.NoStationsFor = m.now().Sub(u.reached)
	if u.state == stateConnected {
		in.LostFor = m.now().Sub(u.reached)
	}
	in.ConnectFailed = u.state == stateConnecting && m.now().Sub(u.reached) > u.client.ConnectTimeout

	d := next(u.state, in)
	if d.State != u.state || d.Action != actNone && d.Action != actSchedule {
		m.log.Info("network manager: wifi", "iface", u.ifname, "from", u.state, "to", d.State, "action", d.Action)
	}
	u.state = d.State
	u.reached = m.now()

	switch d.Action {
	case actConnect:
		m.writeWifiConf(u)
		go u.client.Supervise(ctx)
		go func() {
			if err := u.client.Connect(ctx); err != nil {
				m.log.Warn("network manager: wifi connect failed", "iface", u.ifname, "err", err)
			}
		}()
	case actStartAP:
		if err := u.ap.Start(ctx); err != nil {
			m.log.Error("network manager: start setup AP", "iface", u.ifname, "err", err)
		}
	case actStopAP:
		if err := u.ap.Stop(ctx); err != nil {
			m.log.Error("network manager: stop setup AP", "iface", u.ifname, "err", err)
		}
	}
}

func (m *managed) writeWifiConf(u *wifiUnit) {
	if m.store == nil {
		return
	}
	if err := u.client.WriteConf(); err != nil {
		m.log.Error("network manager: write wpa.conf", "iface", u.ifname, "err", err)
	}
}

func (m *managed) ethernetUp() bool {
	for _, a := range m.addrs() {
		for name := range m.eths {
			if a.Interface == name {
				return true
			}
		}
	}
	return false
}

// savedVisible reports whether a saved SSID was seen in the last scan.
func (m *managed) savedVisible() bool {
	m.mu.Lock()
	nets, at := m.scanNets, m.scanAt
	m.mu.Unlock()
	if len(nets) == 0 || m.now().Sub(at) > 5*time.Minute || m.store == nil {
		return false
	}
	saved := map[string]bool{}
	for _, n := range m.store.List() {
		saved[n.SSID] = true
	}
	for _, n := range nets {
		if saved[n.SSID] {
			return true
		}
	}
	return false
}

// -- Manager interface ------------------------------------------------------

func (m *managed) Status() Status {
	ifaces, err := Discover(m.fsys, m.base)
	if err != nil {
		return Status{}
	}
	addrs := m.addrs()
	var out Status
	for _, info := range ifaces {
		ifc := Interface{Name: info.Name, Kind: info.Kind, State: StateDown}
		for _, a := range addrs {
			if a.Interface == info.Name {
				ifc.State = StateUp
				ifc.IPs = append(ifc.IPs, a.IP)
			}
		}
		m.mu.Lock()
		if u := m.wifis[info.Name]; u != nil {
			ifc.SSID = u.currentSSID()
			if u.state == stateAP {
				ifc.APClients = u.ap.stations()
				ifc.SSID = "WA-Setup"
			}
		}
		m.mu.Unlock()
		out.Interfaces = append(out.Interfaces, ifc)
	}
	return out
}

// InAPMode reports whether any WiFi interface is running the setup AP.
func (m *managed) InAPMode() bool {
	m.mu.Lock()
	defer m.mu.Unlock()
	for _, u := range m.wifis {
		if u.state == stateAP {
			return true
		}
	}
	return false
}

func (m *managed) Scan(ctx context.Context) ([]Network, error) {
	m.mu.Lock()
	var u *wifiUnit
	for _, w := range m.wifis {
		u = w
		break
	}
	m.mu.Unlock()
	if u == nil {
		return nil, fmt.Errorf("no WiFi interface")
	}
	nets, err := u.client.Scan(ctx)
	if err != nil {
		return nil, err
	}
	m.mu.Lock()
	m.scanNets, m.scanAt = nets, m.now()
	m.mu.Unlock()
	return nets, nil
}

func (m *managed) SaveNetwork(ssid, passphrase string) error {
	if m.store == nil {
		return fmt.Errorf("no WiFi store")
	}
	if err := m.store.Save(ssid, passphrase); err != nil {
		return err
	}
	m.mu.Lock()
	for _, u := range m.wifis {
		u.state = stateOff // re-evaluate: try the new network
	}
	m.mu.Unlock()
	return nil
}

func (m *managed) ForgetNetwork(ssid string) error {
	if m.store == nil {
		return fmt.Errorf("no WiFi store")
	}
	if err := m.store.Forget(ssid); err != nil {
		return err
	}
	m.mu.Lock()
	for _, u := range m.wifis {
		_ = u.client.WriteConf()
	}
	m.mu.Unlock()
	return nil
}

func (m *managed) SetAPPolicy(p APPolicy) error {
	switch p {
	case APAuto, APAlways, APNever:
	default:
		return fmt.Errorf("unknown AP policy %q", p)
	}
	m.mu.Lock()
	m.policy = p
	m.mu.Unlock()
	return nil
}

// APPolicy returns the current setup-AP policy.
func (m *managed) APPolicy() APPolicy {
	m.mu.Lock()
	defer m.mu.Unlock()
	return m.policy
}

// SavedNetworks lists the saved WiFi networks.
func (m *managed) SavedNetworks() []SavedNetwork {
	if m.store == nil {
		return nil
	}
	return m.store.List()
}

func (m *managed) Erase() error {
	if m.store == nil {
		return nil
	}
	return m.store.Erase()
}

// currentSSID asks wpa_cli for the SSID we are connected to.
func (u *wifiUnit) currentSSID() string {
	if u.state != stateConnected {
		return ""
	}
	out, err := u.client.Runner.Run(context.Background(), "wpa_cli", "-p", u.client.CtrlDir, "-i", u.ifname, "status")
	if err != nil {
		return ""
	}
	_, ssid := parseWPACliStatus(out)
	return ssid
}

// stations is a tolerant wrapper for the Status page.
func (a *AP) stations() int {
	n, err := a.Stations(context.Background())
	if err != nil {
		return 0
	}
	return n
}
