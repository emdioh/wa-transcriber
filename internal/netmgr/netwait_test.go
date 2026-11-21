package netmgr

import (
	"bytes"
	"context"
	"log/slog"
	"net"
	"os"
	"strings"
	"sync/atomic"
	"testing"
	"time"
)

// logBuffer collects what WaitNetwork writes so a test can assert on the log.
type logBuffer struct{ buf bytes.Buffer }

func (b *logBuffer) Write(p []byte) (int, error) { return b.buf.Write(p) }

func testNetLog() (*slog.Logger, *logBuffer) {
	b := &logBuffer{}
	return slog.New(slog.NewTextHandler(b, nil)), b
}

// staticResolv returns a ReadFile for a fixed resolv.conf content.
func staticResolv(s string) func(string) ([]byte, error) {
	return func(string) ([]byte, error) { return []byte(s), nil }
}

// hasAddressedInterface reports whether this machine has a non-link-local
// IPv4 address, i.e. whether the interface half of the gate can be tested
// here at all.
func hasAddressedInterface() bool { return len(AddressedInterfaces()) > 0 }

func TestUsableNameservers(t *testing.T) {
	if !hasAddressedInterface() {
		t.Skip("no interface with a non-link-local IPv4 address")
	}
	if localResolver(net.IPv6loopback) || localResolver(net.IPv4(127, 0, 0, 1)) {
		t.Skip("a local resolver answers on 127.0.0.1:53")
	}
	cases := []struct {
		name string
		conf string
		want []string
	}{
		{"empty", "", nil},
		{"no nameserver line", "search lan\n", nil},
		{"ipv4", "nameserver 192.168.15.1\n", []string{"192.168.15.1"}},
		{"two", "nameserver 10.0.2.3\nnameserver 8.8.8.8\n", []string{"10.0.2.3", "8.8.8.8"}},
		// The cold-boot state this gate exists for.
		{"ipv6 loopback only", "nameserver ::1\n", nil},
		// busybox udhcpc writes the lease into a temporary file and its
		// script keeps that line as written: no local resolver behind it.
		{"ipv4 loopback only", "nameserver 127.0.0.1\n", nil},
		{"unspecified", "nameserver 0.0.0.0\n", nil},
		{"garbage", "nameserver not-an-ip\n", nil},
		{"loopback plus real", "nameserver ::1\nnameserver 192.168.1.1\n", []string{"192.168.1.1"}},
		{"extra fields", "nameserver\t10.1.2.3 # dhcp\n", []string{"10.1.2.3"}},
	}
	for _, tc := range cases {
		got := UsableNameservers([]byte(tc.conf))
		if strings.Join(got, ",") != strings.Join(tc.want, ",") {
			t.Errorf("%s: UsableNameservers(%q) = %v, want %v", tc.name, tc.conf, got, tc.want)
		}
	}
}

// A loopback server counts only when a local resolver answers it.
func TestUsableNameserverLoopbackNeedsResolver(t *testing.T) {
	if localResolver(net.IPv4(127, 0, 0, 1)) || localResolver(net.IPv6loopback) {
		t.Skip("a local resolver answers on the loopback address")
	}
	for _, ip := range []net.IP{net.IPv4(127, 0, 0, 1), net.IPv6loopback} {
		if UsableNameserver(ip) {
			t.Errorf("%v accepted without a local resolver", ip)
		}
	}
}

func TestUsableNameserver(t *testing.T) {
	cases := []struct {
		name string
		ip   net.IP
		want bool
	}{
		{"nil", nil, false},
		{"unspecified v4", net.IPv4zero, false},
		{"unspecified v6", net.IPv6unspecified, false},
		{"link-local", net.IPv4(169, 254, 1, 1), true},
		{"lan", net.IPv4(192, 168, 15, 1), true},
		{"v6 global", net.ParseIP("2001:db8::1"), true},
	}
	for _, tc := range cases {
		if got := UsableNameserver(tc.ip); got != tc.want {
			t.Errorf("%s: UsableNameserver(%v) = %v, want %v", tc.name, tc.ip, got, tc.want)
		}
	}
}

func TestRoutableIPv4(t *testing.T) {
	cases := []struct {
		ip   string
		want bool
	}{
		{"192.168.15.43", true},
		{"10.0.0.2", true},
		{"127.0.0.1", false},
		{"169.254.12.34", false},
		{"::1", false},
		{"fe80::1", false},
	}
	for _, tc := range cases {
		if got := routableIPv4(net.ParseIP(tc.ip)); got != tc.want {
			t.Errorf("routableIPv4(%s) = %v, want %v", tc.ip, got, tc.want)
		}
	}
}

func TestNetworkUsable(t *testing.T) {
	if !hasAddressedInterface() {
		t.Skip("no interface with a non-link-local IPv4 address")
	}
	ready, why := NetworkUsable(staticResolv("nameserver 192.168.15.1\n"))
	if !ready {
		t.Fatalf("NetworkUsable = false (%s) with an address and a real nameserver", why)
	}
	if ready, why := NetworkUsable(staticResolv("nameserver ::1\n")); ready {
		t.Error("NetworkUsable accepted a ::1 nameserver")
	} else if !strings.Contains(why, "nameserver") {
		t.Errorf("why = %q, want the nameserver to be named", why)
	}
	if ready, why := NetworkUsable(func(string) ([]byte, error) { return nil, os.ErrNotExist }); ready {
		t.Errorf("NetworkUsable accepted a missing resolv.conf (%s)", why)
	}
}

// A cold boot: resolv.conf says ::1 until DHCP writes the lease.
func TestWaitNetworkWaitsForLaterResolver(t *testing.T) {
	if !hasAddressedInterface() {
		t.Skip("no interface with a non-link-local IPv4 address")
	}
	var calls atomic.Int32
	read := func(string) ([]byte, error) {
		if calls.Add(1) < 3 {
			return []byte("nameserver ::1\n"), nil
		}
		return []byte("nameserver 192.168.15.1\n"), nil
	}
	log, logs := testNetLog()
	if !WaitNetwork(context.Background(), NetWait{Managed: true, ReadFile: read, Poll: time.Millisecond, Log: log}) {
		t.Fatal("WaitNetwork gave up while the resolver appeared")
	}
	if calls.Load() < 3 {
		t.Fatalf("returned after %d checks, before the resolver appeared", calls.Load())
	}
	out := logs.buf.String()
	if strings.Count(out, "pairing: waiting for network") != 1 || !strings.Contains(out, "reason=\"resolver has no usable nameserver\"") {
		t.Errorf("want one wait line naming the resolver, got: %s", out)
	}
	if !strings.Contains(out, "pairing: network is usable, connecting") {
		t.Errorf("no ready line logged: %s", out)
	}
}

func TestWaitNetworkReturnsAtOnceWhenUsable(t *testing.T) {
	if !hasAddressedInterface() {
		t.Skip("no interface with a non-link-local IPv4 address")
	}
	log, logs := testNetLog()
	if !WaitNetwork(context.Background(), NetWait{Managed: true, ReadFile: staticResolv("nameserver 8.8.8.8\n"), Log: log}) {
		t.Fatal("WaitNetwork reported an unusable network")
	}
	if logs.buf.Len() != 0 {
		t.Errorf("a usable network should log nothing, got %q", logs.buf.String())
	}
}

// Managed mode with no network at all: a bounded wait, then the pairing retry
// loop takes over exactly as it did before the gate existed.
func TestWaitNetworkManagedDeadline(t *testing.T) {
	if testing.Short() {
		t.Skip("waits ManagedWait")
	}
	start := time.Now()
	log, logs := testNetLog()
	if WaitNetwork(context.Background(), NetWait{Managed: true, ReadFile: staticResolv(""), Poll: 20 * time.Millisecond, Log: log}) {
		t.Fatal("WaitNetwork reported a usable network")
	}
	if waited := time.Since(start); waited < ManagedWait {
		t.Fatalf("returned after %v, want at least %v", waited, ManagedWait)
	}
	out := logs.buf.String()
	if strings.Count(out, "pairing: waiting for network") != 1 {
		t.Errorf("want exactly one wait line, got: %s", out)
	}
	if !strings.Contains(out, "connecting anyway") {
		t.Errorf("no fallback warning: %s", out)
	}
}

func TestWaitNetworkInAPModeUsesShortWait(t *testing.T) {
	if testing.Short() {
		t.Skip("waits APWait")
	}
	start := time.Now()
	log, _ := testNetLog()
	if WaitNetwork(context.Background(), NetWait{
		Managed: true, InAPMode: func() bool { return true },
		ReadFile: staticResolv(""), Poll: 20 * time.Millisecond, Log: log,
	}) {
		t.Fatal("WaitNetwork reported a usable network")
	}
	if waited := time.Since(start); waited < APWait || waited > ManagedWait {
		t.Fatalf("waited %v, want about %v", waited, APWait)
	}
}

func TestWaitNetworkStopsWithContext(t *testing.T) {
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	if WaitNetwork(ctx, NetWait{Managed: true, ReadFile: staticResolv("nameserver ::1\n")}) {
		t.Fatal("WaitNetwork reported a usable network")
	}
}

// Managed off (Docker, dev): the host owns the interface, so nothing here may
// block the first connect on its own; only the caller's context bounds it.
func TestWaitNetworkUnmanagedDoesNotBlock(t *testing.T) {
	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Millisecond)
	defer cancel()
	log, logs := testNetLog()
	start := time.Now()
	if WaitNetwork(ctx, NetWait{ReadFile: staticResolv("nameserver ::1\n"), Poll: time.Millisecond, Log: log}) {
		t.Fatal("WaitNetwork reported a usable network")
	}
	if waited := time.Since(start); waited > time.Second {
		t.Fatalf("unmanaged wait blocked for %v", waited)
	}
	out := logs.buf.String()
	if strings.Count(out, "pairing: waiting for network") != 1 {
		t.Errorf("want exactly one wait line, got: %s", out)
	}
	if strings.Contains(out, "connecting anyway") {
		t.Errorf("unmanaged mode must not log a timeout it does not have: %s", out)
	}
}

func TestWaitNetworkUnmanagedReturnsWhenUsable(t *testing.T) {
	if !hasAddressedInterface() {
		t.Skip("no interface with a non-link-local IPv4 address")
	}
	if !WaitNetwork(context.Background(), NetWait{ReadFile: staticResolv("nameserver 10.0.2.3\n")}) {
		t.Fatal("WaitNetwork reported an unusable network")
	}
}

// The gate sits between the start and the first connect: its budgets must fit
// the 30 s boot-to-web-UI target.
func TestNetWaitBudgetsFitBootBudget(t *testing.T) {
	const bootBudget = 30 * time.Second
	if WaitEvery <= 0 || WaitEvery > 5*time.Second {
		t.Errorf("WaitEvery = %v, want a poll between 0 and 5 s", WaitEvery)
	}
	for name, d := range map[string]time.Duration{"APWait": APWait, "ManagedWait": ManagedWait} {
		if d <= 0 || d > bootBudget {
			t.Errorf("%s = %v, want 0 < d <= %v", name, d, bootBudget)
		}
	}
	if APWait > ManagedWait {
		t.Errorf("APWait %v exceeds ManagedWait %v", APWait, ManagedWait)
	}
	if DefaultResolvConf != "/etc/resolv.conf" {
		t.Errorf("DefaultResolvConf = %q", DefaultResolvConf)
	}
}
