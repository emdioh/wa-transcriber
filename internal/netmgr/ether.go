package netmgr

import (
	"context"
	"crypto/sha256"
	"fmt"
	"log/slog"
	"net"
	"strconv"
	"sync"
	"time"
)

// Ethernet manages one ethernet interface: on carrier up it tries DHCP and
// falls back to an IPv4 link-local address; on carrier down it flushes
// addresses. The carrier signal itself is fed by the caller (see linux.go).
type Ethernet struct {
	IfName string
	MAC    net.HardwareAddr
	Runner Runner
	Log    *slog.Logger

	// After and RetryInterval are injectable for tests.
	After         func(time.Duration) <-chan time.Time
	DHCPTimeout   time.Duration // deadline for one DHCP attempt
	RetryInterval time.Duration // how often DHCP is retried while on link-local

	mu     sync.Mutex
	cancel func()
}

// NewEthernet returns a configured Ethernet controller.
func NewEthernet(ifname string, mac net.HardwareAddr, run Runner, log *slog.Logger) *Ethernet {
	if log == nil {
		log = slog.Default()
	}
	return &Ethernet{
		IfName:        ifname,
		MAC:           mac,
		Runner:        run,
		Log:           log,
		After:         time.After,
		DHCPTimeout:   15 * time.Second,
		RetryInterval: 60 * time.Second,
	}
}

// Up handles a carrier-up event. It returns once the interface is up and a
// DHCP lease or a link-local address is in place. If link-local was chosen,
// a background loop keeps retrying DHCP and drops the link-local address when
// a lease arrives.
func (e *Ethernet) Up(ctx context.Context) {
	retryCtx, cancel := context.WithCancel(ctx)
	e.setCancel(cancel)

	if _, err := e.Runner.Run(ctx, "ip", "link", "set", e.IfName, "up"); err != nil {
		e.Log.Error("ethernet: bring link up", "iface", e.IfName, "err", err)
		return
	}
	if e.tryDHCP(ctx) {
		e.Log.Info("ethernet: dhcp lease", "iface", e.IfName)
		return
	}

	ip, err := e.addLinkLocal(ctx)
	if err != nil {
		e.Log.Error("ethernet: link-local failed", "iface", e.IfName, "err", err)
		return
	}
	e.Log.Info("ethernet: using link-local address", "iface", e.IfName, "ip", ip.String())
	go e.retryDHCP(retryCtx, ip)
}

// Down handles a carrier-down event: it stops any DHCP retry loop and flushes
// the interface's addresses.
func (e *Ethernet) Down(ctx context.Context) error {
	if c := e.takeCancel(); c != nil {
		c()
	}
	_, err := e.Runner.Run(ctx, "ip", "addr", "flush", "dev", e.IfName)
	return err
}

func (e *Ethernet) setCancel(c func()) {
	e.mu.Lock()
	e.cancel = c
	e.mu.Unlock()
}

func (e *Ethernet) takeCancel() func() {
	e.mu.Lock()
	defer e.mu.Unlock()
	c := e.cancel
	e.cancel = nil
	return c
}

// tryDHCP runs one bounded udhcpc attempt and reports whether a lease was
// obtained. -n makes udhcpc exit (0/1) instead of retrying in the
// foreground, so the DHCPTimeout is the outer bound of the attempt.
func (e *Ethernet) tryDHCP(ctx context.Context) bool {
	dctx, cancel := context.WithTimeout(ctx, e.DHCPTimeout)
	defer cancel()
	_, err := e.Runner.Run(dctx, "udhcpc",
		"-n", "-i", e.IfName, "-f", "-q", "-t", "5", "-T", "3",
		"-s", "/usr/share/udhcpc/default.script")
	return err == nil
}

// addLinkLocal picks a free 169.254/16 address with duplicate detection.
func (e *Ethernet) addLinkLocal(ctx context.Context) (net.IP, error) {
	for attempt := 0; attempt < 10; attempt++ {
		ip := linkLocalIP(e.MAC, attempt)
		// arping -D exits 0 when the address is already in use.
		if _, err := e.Runner.Run(ctx, "arping", "-D", "-c", "2", "-I", e.IfName, ip.String()); err == nil {
			continue
		}
		if _, err := e.Runner.Run(ctx, "ip", "addr", "add", ip.String()+"/16", "dev", e.IfName); err != nil {
			return nil, err
		}
		return ip, nil
	}
	return nil, fmt.Errorf("no free link-local address after 10 attempts")
}

// retryDHCP runs until a DHCP lease is obtained (then the link-local address
// is removed) or the context is cancelled.
func (e *Ethernet) retryDHCP(ctx context.Context, linkIP net.IP) {
	timer := e.After(e.RetryInterval)
	for {
		select {
		case <-ctx.Done():
			return
		case <-timer:
			if ctx.Err() != nil {
				return // cancelled while the timer was firing
			}
			if e.tryDHCP(ctx) {
				if _, err := e.Runner.Run(ctx, "ip", "addr", "del", linkIP.String()+"/16", "dev", e.IfName); err != nil {
					e.Log.Error("ethernet: drop link-local address", "iface", e.IfName, "err", err)
					return
				}
				e.Log.Info("ethernet: dhcp lease obtained, link-local dropped", "iface", e.IfName)
				return
			}
			timer = e.After(e.RetryInterval)
		}
	}
}

// linkLocalIP derives a 169.254/16 address from the MAC address (and a
// retry counter for collisions), as described in PLAN.md M5 step 3.
func linkLocalIP(mac net.HardwareAddr, attempt int) net.IP {
	sum := sha256.Sum256([]byte(mac.String() + "|" + strconv.Itoa(attempt)))
	h := uint16(sum[0])<<8 | uint16(sum[1])
	return net.IPv4(169, 254, byte(int(h>>8)%254+1), byte(int(h&0xff)%254+1))
}
