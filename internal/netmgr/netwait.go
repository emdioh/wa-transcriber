package netmgr

import (
	"context"
	"log/slog"
	"net"
	"os"
	"strings"
	"time"
)

// DefaultResolvConf is the file the image's DHCP script (and any host running
// the "none" mode) writes the nameservers to.
const DefaultResolvConf = "/etc/resolv.conf"

// APWait bounds the wait while the setup access point runs, ManagedWait
// bounds it in managed mode, and WaitEvery is the poll interval (PLAN.md M5
// step 10). A board whose network is broken still starts connecting after its
// timeout, as it did before this gate, but the first attempt then no longer
// runs against an empty resolver.
const (
	APWait      = 10 * time.Second
	ManagedWait = 15 * time.Second
	WaitEvery   = 3 * time.Second
)

// NetWait is the network state the wait before the first connect depends on;
// tests replace both functions.
type NetWait struct {
	// Managed selects the bounded wait (device image). Without it the host
	// owns the network (Docker, dev): wait only for a usable network.
	Managed bool
	// ReadFile reads the resolver configuration (nil: os.ReadFile of
	// DefaultResolvConf).
	ReadFile func(string) ([]byte, error)
	// InAPMode reports whether the setup access point is running: nobody is
	// going to configure an uplink until someone joins it.
	InAPMode func() bool
	// Poll is the check interval (0: WaitEvery).
	Poll time.Duration
	Log  *slog.Logger
}

// WaitNetwork blocks until the network is usable, the mode's timeout expires
// or ctx ends, and reports whether it was usable when it returned. It logs
// one line while waiting, so a slow DHCP lease is visible instead of looking
// like a failed pairing attempt against an unprepared resolver.
func WaitNetwork(ctx context.Context, w NetWait) bool {
	if w.Poll <= 0 {
		w.Poll = WaitEvery
	}
	if w.Log == nil {
		w.Log = slog.Default()
	}
	if w.ReadFile == nil {
		w.ReadFile = os.ReadFile
	}
	limit := time.Duration(0)
	waiting := false
	if w.Managed {
		limit = ManagedWait
		if w.InAPMode != nil && w.InAPMode() {
			limit = APWait
		}
	}
	deadline := time.Now().Add(limit) // zero when unmanaged: no deadline
	for {
		if ready, why := NetworkUsable(w.ReadFile); ready {
			if waiting {
				w.Log.Info("pairing: network is usable, connecting")
			}
			return true
		} else if !waiting {
			// One line per wait, not per poll: the boot log stays readable.
			w.Log.Info("pairing: waiting for network", "reason", why, "timeout", limit.String())
			waiting = true
		}
		if limit > 0 && !time.Now().Before(deadline) {
			w.Log.Warn("pairing: network is still not usable, connecting anyway")
			return false
		}
		t := time.NewTimer(w.Poll)
		select {
		case <-ctx.Done():
			t.Stop()
			return false
		case <-t.C:
		}
	}
}

// NetworkUsable reports whether the device can reach a name server: at least
// one interface has a non-link-local IPv4 address and the resolver
// configuration names at least one server we can actually use. why describes
// the first missing piece.
func NetworkUsable(readFile func(string) ([]byte, error)) (ready bool, why string) {
	if readFile == nil {
		readFile = os.ReadFile
	}
	if len(AddressedInterfaces()) == 0 {
		return false, "no interface has an IPv4 address"
	}
	b, err := readFile(DefaultResolvConf)
	if err != nil {
		return false, "no resolver configured"
	}
	servers := UsableNameservers(b)
	if len(servers) == 0 {
		return false, "resolver has no usable nameserver"
	}
	return true, ""
}

// AddressedInterfaces lists the non-loopback interfaces that are up and have
// a non-link-local IPv4 address (169.254.0.0/16 does not route anywhere).
func AddressedInterfaces() []net.Interface {
	ifaces, err := net.Interfaces()
	if err != nil {
		return nil
	}
	var out []net.Interface
	for _, ifc := range ifaces {
		if ifc.Flags&net.FlagUp == 0 || ifc.Flags&net.FlagLoopback != 0 {
			continue
		}
		addrs, err := ifc.Addrs()
		if err != nil {
			continue
		}
		for _, a := range addrs {
			ipn, ok := a.(*net.IPNet)
			if ok && routableIPv4(ipn.IP) {
				out = append(out, ifc)
				break
			}
		}
	}
	return out
}

// routableIPv4 reports whether ip is a 4-byte IPv4 address that is neither
// loopback nor link-local.
func routableIPv4(ip net.IP) bool {
	v4 := ip.To4()
	return v4 != nil && !v4.IsLoopback() && !v4.IsLinkLocalUnicast()
}

// UsableNameservers parses a resolv.conf and returns the name servers worth
// asking.
//
// A loopback server only works when something local answers it: busybox
// udhcpc writes "nameserver 127.0.0.1" into a temporary file, sets some
// environment variables and then hands that file to its script unchanged, so
// a device can end up with a loopback server and no local resolver at all —
// and "nameserver ::1" is left from the boot state of the image. Getting this
// wrong is what makes the first pairing attempt fail with
// "lookup web.whatsapp.com on [::1]:53: connect: cannot assign requested
// address".
func UsableNameservers(b []byte) []string {
	var out []string
	for _, line := range strings.Split(string(b), "\n") {
		if f := strings.Fields(line); len(f) >= 2 && f[0] == "nameserver" {
			if ip := net.ParseIP(f[1]); ip != nil && UsableNameserver(ip) {
				out = append(out, f[1])
			}
		}
	}
	return out
}

// UsableNameserver reports whether ip is a name server we can use right now:
// not unspecified, and not loopback unless a local resolver answers there.
func UsableNameserver(ip net.IP) bool {
	if ip == nil || ip.IsUnspecified() {
		return false
	}
	if !ip.IsLoopback() {
		return true
	}
	return localResolver(ip)
}

// localResolver reports whether a local name server answers on the DNS port
// of a loopback address. It changes nothing and only talks to the local host.
//
// It sends a DNS query and insists on a reply: a UDP dial succeeds even
// without a listener, and reading the TCP listen table (/proc/net/tcp) is
// blind to sockets bound inside a network namespace, so neither is a usable
// probe. A real query is answered by busybox dnsmasq, systemd-resolved and
// unbound alike, and a quiet 200 ms is fine because this runs once per poll
// of a bounded wait.
func localResolver(ip net.IP) bool {
	conn, err := net.DialTimeout("udp", net.JoinHostPort(ip.String(), "53"), 200*time.Millisecond)
	if err != nil {
		return false
	}
	defer conn.Close()
	// One header-only query, id 0, "a." A, IN; the reply only has to arrive.
	conn.SetDeadline(time.Now().Add(200 * time.Millisecond))
	if _, err := conn.Write([]byte{0, 0, 0x01, 0x00, 0, 1, 0, 0, 0, 0, 0, 0, 1, 'a', 0, 0, 1, 0, 1}); err != nil {
		return false
	}
	var buf [512]byte
	n, err := conn.Read(buf[:])
	return err == nil && n >= 12
}
