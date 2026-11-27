package netmgr

import (
	"context"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"net"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/emdioh/wa-transcriber/internal/netmgr/apsrv"
	"github.com/emdioh/wa-transcriber/internal/netmgr/dhcpc"
	"github.com/emdioh/wa-transcriber/internal/netmgr/netops"
)

type fakeRunner struct {
	mu       sync.Mutex
	calls    []string
	script   map[string][]error
	outputs  map[string][]string
	startErr error
	stopped  int
}

func newFakeRunner(script map[string][]error) *fakeRunner {
	return &fakeRunner{script: script, outputs: map[string][]string{}}
}

func (f *fakeRunner) Run(_ context.Context, name string, args ...string) ([]byte, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.calls = append(f.calls, name+" "+strings.Join(args, " "))
	var out []byte
	if list := f.outputs[name]; len(list) > 0 {
		out = []byte(list[0])
		f.outputs[name] = list[1:]
	}
	var err error
	if list := f.script[name]; len(list) > 0 {
		err = list[0]
		f.script[name] = list[1:]
	}
	return out, err
}

func (f *fakeRunner) Start(_ context.Context, name string, args ...string) (func(), func() error, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.calls = append(f.calls, name+" "+strings.Join(args, " "))
	if f.startErr != nil {
		return nil, nil, f.startErr
	}
	stop := func() {
		f.mu.Lock()
		f.stopped++
		f.mu.Unlock()
	}
	return stop, func() error { return nil }, nil
}

func (f *fakeRunner) snapshot() []string {
	f.mu.Lock()
	defer f.mu.Unlock()
	return append([]string(nil), f.calls...)
}

func (f *fakeRunner) stopCount() int {
	f.mu.Lock()
	defer f.mu.Unlock()
	return f.stopped
}

func (f *fakeRunner) waitFor(t *testing.T, substr string) []string {
	t.Helper()
	deadline := time.Now().Add(3 * time.Second)
	for {
		calls := f.snapshot()
		for _, c := range calls {
			if strings.Contains(c, substr) {
				return calls
			}
		}
		if time.Now().After(deadline) {
			t.Fatalf("command %q never ran; calls: %v", substr, calls)
		}
		time.Sleep(5 * time.Millisecond)
	}
}

var _ netops.Ops = (*fakeOps)(nil)

type fakeOps struct {
	mu           sync.Mutex
	calls        []string
	arpConflicts []bool
	arpErrs      []error
	stations     int
	stationsErr  error
	errs         map[string]error
}

func newFakeOps() *fakeOps {
	return &fakeOps{errs: map[string]error{}}
}

func (f *fakeOps) record(op, call string) error {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.calls = append(f.calls, call)
	return f.errs[op]
}

func (f *fakeOps) LinkUp(ifname string) error {
	return f.record("LinkUp", "link up "+ifname)
}

func (f *fakeOps) LinkDown(ifname string) error {
	return f.record("LinkDown", "link down "+ifname)
}

func (f *fakeOps) AddrAdd(ifname string, ip net.IP, prefixLen int) error {
	return f.record("AddrAdd", fmt.Sprintf("addr add %s/%d dev %s", ip, prefixLen, ifname))
}

func (f *fakeOps) AddrReplace(ifname string, ip net.IP, prefixLen int) error {
	return f.record("AddrReplace", fmt.Sprintf("addr replace %s/%d dev %s", ip, prefixLen, ifname))
}

func (f *fakeOps) AddrDel(ifname string, ip net.IP, prefixLen int) error {
	return f.record("AddrDel", fmt.Sprintf("addr del %s/%d dev %s", ip, prefixLen, ifname))
}

func (f *fakeOps) AddrFlush(ifname string) error {
	return f.record("AddrFlush", "addr flush dev "+ifname)
}

func (f *fakeOps) AddrReplacePrimary(ifname string, ip net.IP, prefixLen int) error {
	return f.record("AddrReplacePrimary", fmt.Sprintf("addr replace-primary %s/%d dev %s", ip, prefixLen, ifname))
}

func (f *fakeOps) RouteReplaceDefault(ifname string, gw net.IP) error {
	return f.record("RouteReplaceDefault", fmt.Sprintf("route replace default via %s dev %s", gw, ifname))
}

func (f *fakeOps) ARPProbe(_ context.Context, ifname string, _ net.HardwareAddr, ip net.IP) (bool, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.calls = append(f.calls, fmt.Sprintf("arp probe %s %s", ifname, ip))
	var err error
	if len(f.arpErrs) > 0 {
		err = f.arpErrs[0]
		f.arpErrs = f.arpErrs[1:]
	}
	var conflict bool
	if len(f.arpConflicts) > 0 {
		conflict = f.arpConflicts[0]
		f.arpConflicts = f.arpConflicts[1:]
	}
	return conflict, err
}

func (f *fakeOps) StationCount(ifname string) (int, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.calls = append(f.calls, "station count "+ifname)
	return f.stations, f.stationsErr
}

func (f *fakeOps) snapshot() []string {
	f.mu.Lock()
	defer f.mu.Unlock()
	return append([]string(nil), f.calls...)
}

func (f *fakeOps) waitFor(t *testing.T, substr string) []string {
	t.Helper()
	deadline := time.Now().Add(3 * time.Second)
	for {
		calls := f.snapshot()
		for _, c := range calls {
			if strings.Contains(c, substr) {
				return calls
			}
		}
		if time.Now().After(deadline) {
			t.Fatalf("netops call %q never ran; calls: %v", substr, calls)
		}
		time.Sleep(5 * time.Millisecond)
	}
}

type fakeDHCP struct {
	mu      sync.Mutex
	calls   []string
	results []bool
	stopped int
}

func newFakeDHCP(results ...bool) *fakeDHCP {
	return &fakeDHCP{results: results}
}

func (f *fakeDHCP) fn() DHCPFunc {
	return func(_ context.Context, ifname string, mac net.HardwareAddr, _ time.Duration) (bool, func()) {
		f.mu.Lock()
		f.calls = append(f.calls, fmt.Sprintf("dhcp %s %s", ifname, mac))
		ok := true
		if len(f.results) > 0 {
			ok = f.results[0]
			f.results = f.results[1:]
		}
		f.mu.Unlock()
		if !ok {
			return false, nil
		}
		return true, func() {
			f.mu.Lock()
			f.stopped++
			f.mu.Unlock()
		}
	}
}

func (f *fakeDHCP) snapshot() []string {
	f.mu.Lock()
	defer f.mu.Unlock()
	return append([]string(nil), f.calls...)
}

func (f *fakeDHCP) stopCount() int {
	f.mu.Lock()
	defer f.mu.Unlock()
	return f.stopped
}

func (f *fakeDHCP) waitFor(t *testing.T, substr string) []string {
	t.Helper()
	deadline := time.Now().Add(3 * time.Second)
	for {
		calls := f.snapshot()
		for _, c := range calls {
			if strings.Contains(c, substr) {
				return calls
			}
		}
		if time.Now().After(deadline) {
			t.Fatalf("dhcp call %q never ran; calls: %v", substr, calls)
		}
		time.Sleep(5 * time.Millisecond)
	}
}

type fakeWPACtrl struct {
	mu      sync.Mutex
	calls   []string
	replies map[string][][]byte
	errs    map[string][]error
}

func newFakeWPACtrl() *fakeWPACtrl {
	return &fakeWPACtrl{
		replies: map[string][][]byte{},
		errs:    map[string][]error{},
	}
}

func (f *fakeWPACtrl) Command(_ context.Context, cmd string) ([]byte, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.calls = append(f.calls, cmd)
	var err error
	if list := f.errs[cmd]; len(list) > 0 {
		err = list[0]
		f.errs[cmd] = list[1:]
	}
	var out []byte
	if list := f.replies[cmd]; len(list) > 0 {
		out = list[0]
		f.replies[cmd] = list[1:]
	}
	return out, err
}

func (f *fakeWPACtrl) snapshot() []string {
	f.mu.Lock()
	defer f.mu.Unlock()
	return append([]string(nil), f.calls...)
}

type fakeAPServer struct {
	mu       sync.Mutex
	configs  []apsrv.Config
	startErr error
	closed   int
}

func (f *fakeAPServer) start() APServerStart {
	return func(_ context.Context, cfg apsrv.Config) (func() error, error) {
		f.mu.Lock()
		f.configs = append(f.configs, cfg)
		err := f.startErr
		f.mu.Unlock()
		if err != nil {
			return nil, err
		}
		return func() error {
			f.mu.Lock()
			f.closed++
			f.mu.Unlock()
			return nil
		}, nil
	}
}

func (f *fakeAPServer) snapshot() []apsrv.Config {
	f.mu.Lock()
	defer f.mu.Unlock()
	return append([]apsrv.Config(nil), f.configs...)
}

func (f *fakeAPServer) closeCount() int {
	f.mu.Lock()
	defer f.mu.Unlock()
	return f.closed
}

func testLogger() *slog.Logger {
	return slog.New(slog.NewTextHandler(io.Discard, nil))
}

func testMAC() net.HardwareAddr {
	mac, _ := net.ParseMAC("b8:27:eb:00:11:22")
	return mac
}

func TestEthernetDHCPLease(t *testing.T) {
	ops := newFakeOps()
	dhcp := newFakeDHCP(true)
	e := NewEthernet("eth0", testMAC(), ops, testLogger())
	e.DHCP = dhcp.fn()
	e.Up(context.Background())

	opsCalls := ops.snapshot()
	if len(opsCalls) != 1 || opsCalls[0] != "link up eth0" {
		t.Fatalf("ops calls = %v, want [link up eth0]", opsCalls)
	}
	dhcpCalls := dhcp.snapshot()
	if len(dhcpCalls) != 1 || dhcpCalls[0] != "dhcp eth0 "+testMAC().String() {
		t.Fatalf("dhcp calls = %v", dhcpCalls)
	}
	if err := e.Down(context.Background()); err != nil {
		t.Fatalf("Down: %v", err)
	}
	if got := dhcp.stopCount(); got != 1 {
		t.Fatalf("DHCP renewal stop count = %d, want 1", got)
	}
}

func TestEthernetFallsBackToLinkLocal(t *testing.T) {
	ops := newFakeOps()
	ops.arpConflicts = []bool{false}
	dhcp := newFakeDHCP(false)
	e := NewEthernet("eth0", testMAC(), ops, testLogger())
	e.DHCP = dhcp.fn()
	e.After = func(time.Duration) <-chan time.Time { return make(chan time.Time) }
	e.Up(context.Background())

	calls := ops.snapshot()
	if len(calls) != 3 {
		t.Fatalf("ops calls = %v, want 3", calls)
	}
	want := linkLocalIP(testMAC(), 0).String()
	if calls[0] != "link up eth0" {
		t.Errorf("calls[0] = %q", calls[0])
	}
	if calls[1] != "arp probe eth0 "+want {
		t.Errorf("arp probe call = %q", calls[1])
	}
	if calls[2] != "addr add "+want+"/16 dev eth0" {
		t.Errorf("addr add call = %q", calls[2])
	}

	if err := e.Down(context.Background()); err != nil {
		t.Fatalf("Down: %v", err)
	}
	if got := ops.snapshot(); got[len(got)-1] == "addr flush dev eth0" {
		t.Errorf("a carrier-down event flushed the address: %v", got)
	}

	if err := e.Flush(context.Background()); err != nil {
		t.Fatalf("Flush: %v", err)
	}
	last := ops.snapshot()[len(ops.snapshot())-1]
	if last != "addr flush dev eth0" {
		t.Errorf("last call after Flush = %q", last)
	}
}

func TestEthernetLinkLocalCollisionRetries(t *testing.T) {
	ops := newFakeOps()
	ops.arpConflicts = []bool{true /* duplicate */, false}
	dhcp := newFakeDHCP(false)
	e := NewEthernet("eth0", testMAC(), ops, testLogger())
	e.DHCP = dhcp.fn()
	e.After = func(time.Duration) <-chan time.Time { return make(chan time.Time) }
	e.Up(context.Background())

	calls := ops.snapshot()
	if len(calls) != 4 {
		t.Fatalf("ops calls = %v, want 4", calls)
	}
	first := linkLocalIP(testMAC(), 0).String()
	second := linkLocalIP(testMAC(), 1).String()
	if calls[1] != "arp probe eth0 "+first {
		t.Errorf("first arp probe = %q", calls[1])
	}
	if calls[2] != "arp probe eth0 "+second {
		t.Errorf("second arp probe = %q", calls[2])
	}
	if calls[3] != "addr add "+second+"/16 dev eth0" {
		t.Errorf("addr add = %q", calls[3])
	}
}

func TestEthernetRetryDHCPDropsLinkLocal(t *testing.T) {
	ops := newFakeOps()
	ops.arpConflicts = []bool{false}
	dhcp := newFakeDHCP(false, true)
	e := NewEthernet("eth0", testMAC(), ops, testLogger())
	e.DHCP = dhcp.fn()
	ch := make(chan time.Time)
	e.After = func(time.Duration) <-chan time.Time { return ch }
	e.Up(context.Background())

	close(ch) // fires the retry timer; the second DHCP attempt succeeds
	calls := ops.waitFor(t, "addr del")
	want := "addr del " + linkLocalIP(testMAC(), 0).String() + "/16 dev eth0"
	found := false
	for _, c := range calls {
		if c == want {
			found = true
		}
	}
	if !found {
		t.Fatalf("%q not found; calls: %v", want, calls)
	}
	if err := e.Down(context.Background()); err != nil {
		t.Fatalf("Down: %v", err)
	}
	if got := dhcp.stopCount(); got != 1 {
		t.Fatalf("DHCP renewal stop count = %d, want 1", got)
	}
}

func TestEthernetDownStopsRetries(t *testing.T) {
	ops := newFakeOps()
	ops.arpConflicts = []bool{false}
	dhcp := newFakeDHCP(false)
	e := NewEthernet("eth0", testMAC(), ops, testLogger())
	e.DHCP = dhcp.fn()
	ch := make(chan time.Time)
	e.After = func(time.Duration) <-chan time.Time { return ch }
	e.Up(context.Background())
	if err := e.Down(context.Background()); err != nil {
		t.Fatalf("Down: %v", err)
	}

	// The retry loop is cancelled: firing the timer must not run DHCP again.
	before := len(dhcp.snapshot())
	close(ch)
	time.Sleep(20 * time.Millisecond)
	after := len(dhcp.snapshot())
	if after != before {
		t.Fatalf("retry ran after Down: %v", dhcp.snapshot())
	}
}

// A carrier blip must not disturb the address: no flush on the way down, no
// administrative link down, and DHCP runs again on carrier up while stopping
// the prior renewal loop without releasing or flushing (D16).
func TestEthernetCarrierBlipKeepsAddress(t *testing.T) {
	ops := newFakeOps()
	dhcp := newFakeDHCP(true, true)
	e := NewEthernet("eth0", testMAC(), ops, testLogger())
	e.DHCP = dhcp.fn()

	e.Up(context.Background())
	if err := e.Down(context.Background()); err != nil {
		t.Fatalf("Down: %v", err)
	}
	e.Up(context.Background())

	opsCalls := ops.snapshot()
	for _, c := range opsCalls {
		if strings.Contains(c, "addr flush") {
			t.Fatalf("a carrier blip flushed the address: %v", opsCalls)
		}
		if strings.Contains(c, "link down") {
			t.Fatalf("a carrier blip brought the link down: %v", opsCalls)
		}
	}
	if len(opsCalls) != 2 {
		t.Fatalf("ops calls = %v, want 2 (link up, link up)", opsCalls)
	}
	dhcpCalls := dhcp.snapshot()
	if len(dhcpCalls) != 2 {
		t.Fatalf("dhcp calls = %v, want 2", dhcpCalls)
	}
	if dhcpCalls[0] != dhcpCalls[1] {
		t.Errorf("dhcp changed between carrier-up events: %q vs %q", dhcpCalls[0], dhcpCalls[1])
	}
	if got := dhcp.stopCount(); got != 1 {
		t.Errorf("first renewal loop stop count = %d, want 1", got)
	}
}

// applyDHCPLease uses AddrReplacePrimary (D16 idempotent replace, never flush),
// installs the default route when a router is provided, and writes resolv.conf.
func TestApplyDHCPLease(t *testing.T) {
	ops := newFakeOps()
	resolv := filepath.Join(t.TempDir(), "resolv.conf")
	lease := dhcpc.Lease{
		IP:        net.IPv4(192, 168, 1, 50),
		PrefixLen: 24,
		Router:    net.IPv4(192, 168, 1, 1),
		DNS:       []net.IP{net.IPv4(1, 1, 1, 1), net.IPv4(8, 8, 8, 8)},
	}
	if err := applyDHCPLease(ops, "eth0", resolv, lease); err != nil {
		t.Fatalf("applyDHCPLease: %v", err)
	}

	wantOps := []string{
		"addr replace-primary 192.168.1.50/24 dev eth0",
		"route replace default via 192.168.1.1 dev eth0",
	}
	gotOps := ops.snapshot()
	if len(gotOps) != len(wantOps) || gotOps[0] != wantOps[0] || gotOps[1] != wantOps[1] {
		t.Fatalf("ops calls = %v, want %v", gotOps, wantOps)
	}
	data, err := os.ReadFile(resolv)
	if err != nil {
		t.Fatalf("read resolv.conf: %v", err)
	}
	if !strings.Contains(string(data), "nameserver 1.1.1.1\nnameserver 8.8.8.8\n") {
		t.Fatalf("resolv.conf = %q", string(data))
	}

	// Lease without router or DNS only replaces primary address.
	ops2 := newFakeOps()
	resolv2 := filepath.Join(t.TempDir(), "resolv2.conf")
	if err := applyDHCPLease(ops2, "eth0", resolv2, dhcpc.Lease{
		IP:        net.IPv4(10, 0, 0, 9),
		PrefixLen: 24,
	}); err != nil {
		t.Fatalf("applyDHCPLease (no router/dns): %v", err)
	}
	if got := ops2.snapshot(); len(got) != 1 || got[0] != "addr replace-primary 10.0.0.9/24 dev eth0" {
		t.Fatalf("ops2 calls = %v", got)
	}
	if _, err := os.Stat(resolv2); !errors.Is(err, os.ErrNotExist) {
		t.Fatalf("resolv2.conf should not be created when DNS is empty, err = %v", err)
	}
}

// Leaving managed mode must still be able to hand a clean interface over.
func TestEthernetFlushRemovesAddress(t *testing.T) {
	ops := newFakeOps()
	dhcp := newFakeDHCP(true)
	e := NewEthernet("eth0", testMAC(), ops, testLogger())
	e.DHCP = dhcp.fn()
	e.Up(context.Background())
	if err := e.Flush(context.Background()); err != nil {
		t.Fatalf("Flush: %v", err)
	}
	calls := ops.snapshot()
	if calls[len(calls)-1] != "addr flush dev eth0" {
		t.Fatalf("last call = %q, want a flush", calls[len(calls)-1])
	}
	if got := dhcp.stopCount(); got != 1 {
		t.Fatalf("DHCP renewal stop count = %d, want 1", got)
	}
}

// LinkDownOnCarrierLoss is the opt-in for drivers that need the link taken
// down; it takes the link down but still keeps the address.
func TestEthernetDownCanTakeLinkDown(t *testing.T) {
	ops := newFakeOps()
	e := NewEthernet("eth0", testMAC(), ops, testLogger())
	e.LinkDownOnCarrierLoss = true
	if err := e.Down(context.Background()); err != nil {
		t.Fatalf("Down: %v", err)
	}
	calls := ops.snapshot()
	if len(calls) != 1 || calls[0] != "link down eth0" {
		t.Fatalf("calls = %v, want only link down", calls)
	}
}

// missingInterfaceName is a name no interface can have on Linux (15 chars is
// the limit, and every real one is shorter).
func missingInterfaceName(t *testing.T) string {
	t.Helper()
	return "wat-nope-0123456"
}

func TestLinkLocalIPRangeAndDeterminism(t *testing.T) {
	a := linkLocalIP(testMAC(), 0)
	b := linkLocalIP(testMAC(), 0)
	if !a.Equal(b) {
		t.Fatalf("not deterministic: %v vs %v", a, b)
	}
	ip := a.To4()
	if ip == nil || ip[0] != 169 || ip[1] != 254 || ip[2] == 0 || ip[2] == 255 || ip[3] == 0 || ip[3] == 255 {
		t.Fatalf("out of range: %v", a)
	}
}
