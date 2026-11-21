package netmgr

import (
	"context"
	"errors"
	"io"
	"log/slog"
	"net"
	"strings"
	"sync"
	"testing"
	"time"
)

var (
	errNoLease    = errors.New("no lease")
	errNoConflict = errors.New("not in use") // arping -D exit 1
)

type fakeRunner struct {
	mu       sync.Mutex
	calls    []string
	script   map[string][]error
	outputs  map[string][]string
	startErr error
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
	return func() {}, func() error { return nil }, nil
}

func (f *fakeRunner) snapshot() []string {
	f.mu.Lock()
	defer f.mu.Unlock()
	return append([]string(nil), f.calls...)
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

func testLogger() *slog.Logger {
	return slog.New(slog.NewTextHandler(io.Discard, nil))
}

func testMAC() net.HardwareAddr {
	mac, _ := net.ParseMAC("b8:27:eb:00:11:22")
	return mac
}

func TestEthernetDHCPLease(t *testing.T) {
	r := newFakeRunner(map[string][]error{"udhcpc": {nil}})
	e := NewEthernet("eth0", testMAC(), r, testLogger())
	e.Up(context.Background())

	calls := r.snapshot()
	if len(calls) != 2 {
		t.Fatalf("calls = %v, want 2", calls)
	}
	if !strings.HasPrefix(calls[0], "ip link set eth0 up") {
		t.Errorf("first call = %q", calls[0])
	}
	if !strings.Contains(calls[1], "udhcpc -n -i eth0 -f -q -t 5 -T 3 -s /usr/share/udhcpc/default.script") {
		t.Errorf("second call = %q", calls[1])
	}
}

func TestEthernetFallsBackToLinkLocal(t *testing.T) {
	r := newFakeRunner(map[string][]error{
		"udhcpc": {errNoLease},
		"arping": {errNoConflict},
	})
	e := NewEthernet("eth0", testMAC(), r, testLogger())
	e.After = func(time.Duration) <-chan time.Time { return make(chan time.Time) }
	e.Up(context.Background())

	calls := r.snapshot()
	if len(calls) != 4 {
		t.Fatalf("calls = %v, want 4", calls)
	}
	want := linkLocalIP(testMAC(), 0).String()
	if calls[2] != "arping -D -c 2 -I eth0 "+want {
		t.Errorf("arping call = %q", calls[2])
	}
	if calls[3] != "ip addr add "+want+"/16 dev eth0" {
		t.Errorf("ip addr add call = %q", calls[3])
	}

	if err := e.Down(context.Background()); err != nil {
		t.Fatalf("Down: %v", err)
	}
	if got := r.snapshot(); got[len(got)-1] == "ip addr flush dev eth0" {
		t.Errorf("a carrier-down event flushed the address: %v", got)
	}

	if err := e.Flush(context.Background()); err != nil {
		t.Fatalf("Flush: %v", err)
	}
	last := r.snapshot()[len(r.snapshot())-1]
	if last != "ip addr flush dev eth0" {
		t.Errorf("last call after Flush = %q", last)
	}
}

func TestEthernetLinkLocalCollisionRetries(t *testing.T) {
	r := newFakeRunner(map[string][]error{
		"udhcpc": {errNoLease},
		"arping": {nil /* duplicate */, errNoConflict},
	})
	e := NewEthernet("eth0", testMAC(), r, testLogger())
	e.After = func(time.Duration) <-chan time.Time { return make(chan time.Time) }
	e.Up(context.Background())

	calls := r.snapshot()
	if len(calls) != 5 {
		t.Fatalf("calls = %v, want 5", calls)
	}
	first := linkLocalIP(testMAC(), 0).String()
	second := linkLocalIP(testMAC(), 1).String()
	if calls[2] != "arping -D -c 2 -I eth0 "+first {
		t.Errorf("first arping = %q", calls[2])
	}
	if calls[3] != "arping -D -c 2 -I eth0 "+second {
		t.Errorf("second arping = %q", calls[3])
	}
	if calls[4] != "ip addr add "+second+"/16 dev eth0" {
		t.Errorf("ip addr add = %q", calls[4])
	}
}

func TestEthernetRetryDHCPDropsLinkLocal(t *testing.T) {
	r := newFakeRunner(map[string][]error{
		"udhcpc": {errNoLease, nil},
		"arping": {errNoConflict},
	})
	e := NewEthernet("eth0", testMAC(), r, testLogger())
	ch := make(chan time.Time)
	e.After = func(time.Duration) <-chan time.Time { return ch }
	e.Up(context.Background())

	close(ch) // fires the retry timer; the second udhcpc attempt succeeds
	calls := r.waitFor(t, "ip addr del")
	want := linkLocalIP(testMAC(), 0).String() + "/16 dev eth0"
	found := false
	for _, c := range calls {
		if c == "ip addr del "+want {
			found = true
		}
	}
	if !found {
		t.Fatalf("ip addr del %q not found; calls: %v", want, calls)
	}
}

func TestEthernetDownStopsRetries(t *testing.T) {
	r := newFakeRunner(map[string][]error{
		"udhcpc": {errNoLease},
		"arping": {errNoConflict},
	})
	e := NewEthernet("eth0", testMAC(), r, testLogger())
	ch := make(chan time.Time)
	e.After = func(time.Duration) <-chan time.Time { return ch }
	e.Up(context.Background())
	if err := e.Down(context.Background()); err != nil {
		t.Fatalf("Down: %v", err)
	}

	// The retry loop is cancelled: firing the timer must not run udhcpc again.
	before := len(r.snapshot())
	close(ch)
	time.Sleep(20 * time.Millisecond)
	after := len(r.snapshot())
	if after != before {
		t.Fatalf("retry ran after Down: %v", r.snapshot())
	}
}

// A carrier blip must not disturb the address: no flush on the way down, and
// DHCP on the way back up runs the same client as before, with the same
// arguments (no release flag), so our script can keep the address under a live
// TCP connection.
func TestEthernetCarrierBlipKeepsAddress(t *testing.T) {
	r := newFakeRunner(map[string][]error{"udhcpc": {nil, nil}})
	e := NewEthernet("eth0", testMAC(), r, testLogger())

	e.Up(context.Background())
	if err := e.Down(context.Background()); err != nil {
		t.Fatalf("Down: %v", err)
	}
	e.Up(context.Background())

	calls := r.snapshot()
	for _, c := range calls {
		if strings.Contains(c, "ip addr flush") {
			t.Fatalf("a carrier blip flushed the address: %v", calls)
		}
		if strings.Contains(c, "ip link set") && strings.HasSuffix(c, " down") {
			t.Fatalf("a carrier blip brought the link down: %v", calls)
		}
	}
	if len(calls) != 4 {
		t.Fatalf("calls = %v, want 4 (up, dhcp, up, dhcp)", calls)
	}
	if calls[1] != calls[3] {
		t.Errorf("dhcp changed between the two carrier-up events: %q vs %q", calls[1], calls[3])
	}
}

// udhcpc must never be told to release the address on exit: -R means
// "Release IP on exit" (busybox udhcpc --help) and its script then runs
// deconfig, which is exactly the teardown a carrier blip must avoid.
func TestDHCPLeaseNeverReleases(t *testing.T) {
	r := newFakeRunner(map[string][]error{"udhcpc": {nil}})
	if !dhcpLease(context.Background(), r, "eth0", time.Second) {
		t.Fatal("dhcpLease reported no lease")
	}
	calls := r.snapshot()
	if len(calls) != 1 {
		t.Fatalf("calls = %v, want 1", calls)
	}
	for _, field := range strings.Fields(calls[0]) {
		if field == "-R" || field == "--release" {
			t.Errorf("udhcpc got a release flag: %q", calls[0])
		}
	}
	if !strings.Contains(calls[0], "-s /usr/share/udhcpc/default.script") {
		t.Errorf("udhcpc did not use our script: %q", calls[0])
	}
}

// Leaving managed mode must still be able to hand a clean interface over.
func TestEthernetFlushRemovesAddress(t *testing.T) {
	r := newFakeRunner(map[string][]error{"udhcpc": {nil}})
	e := NewEthernet("eth0", testMAC(), r, testLogger())
	e.Up(context.Background())
	if err := e.Flush(context.Background()); err != nil {
		t.Fatalf("Flush: %v", err)
	}
	calls := r.snapshot()
	if calls[len(calls)-1] != "ip addr flush dev eth0" {
		t.Fatalf("last call = %q, want a flush", calls[len(calls)-1])
	}
}

// LinkDownOnCarrierLoss is the opt-in for drivers that need the link taken
// down; it takes the link down but still keeps the address.
func TestEthernetDownCanTakeLinkDown(t *testing.T) {
	r := newFakeRunner(nil)
	e := NewEthernet("eth0", testMAC(), r, testLogger())
	e.LinkDownOnCarrierLoss = true
	if err := e.Down(context.Background()); err != nil {
		t.Fatalf("Down: %v", err)
	}
	calls := r.snapshot()
	if len(calls) != 1 || calls[0] != "ip link set eth0 down" {
		t.Fatalf("calls = %v, want only the link down", calls)
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
