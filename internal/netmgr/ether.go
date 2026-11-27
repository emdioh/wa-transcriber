package netmgr

import (
	"context"
	"crypto/sha256"
	"fmt"
	"log/slog"
	"net"
	"os"
	"strconv"
	"sync"
	"time"

	"github.com/emdioh/wa-transcriber/internal/netmgr/dhcpc"
	"github.com/emdioh/wa-transcriber/internal/netmgr/netops"
)

// DHCPFunc obtains an IPv4 DHCP lease on ifname within timeout and, on success,
// starts background lease renewal stopped by the returned stop function.
// Stopping renewal must never release or remove the interface's address (D16).
type DHCPFunc func(ctx context.Context, ifname string, mac net.HardwareAddr, timeout time.Duration) (ok bool, stop func())

// applyDHCPLease configures ifname with lease without dropping an unchanged
// primary IPv4 address (D16) and updates resolvConf when DNS servers are given.
func applyDHCPLease(ops netops.Ops, ifname, resolvConf string, lease dhcpc.Lease) error {
	if err := ops.AddrReplacePrimary(ifname, lease.IP, lease.PrefixLen); err != nil {
		return err
	}
	if lease.Router != nil {
		if err := ops.RouteReplaceDefault(ifname, lease.Router); err != nil {
			return err
		}
	}
	if resolvConf != "" && len(lease.DNS) > 0 {
		if err := dhcpc.WriteResolvConf(resolvConf, lease.DNS); err != nil {
			return err
		}
	}
	return nil
}

// defaultDHCPFunc runs the pure-Go DHCPv4 client (dhcpc.Obtain + dhcpc.Maintain)
// and applies acquired and renewed leases via ops.
func defaultDHCPFunc(ops netops.Ops, resolvConf string, log *slog.Logger) DHCPFunc {
	return func(ctx context.Context, ifname string, mac net.HardwareAddr, timeout time.Duration) (bool, func()) {
		hostname, _ := os.Hostname()
		opts := dhcpc.Options{
			IfName:     ifname,
			MAC:        mac,
			Hostname:   hostname,
			Timeout:    timeout,
			ResolvConf: resolvConf,
			Log:        log,
			Apply: func(l dhcpc.Lease) error {
				return applyDHCPLease(ops, ifname, resolvConf, l)
			},
		}
		lease, err := dhcpc.Obtain(ctx, opts)
		if err != nil {
			if log != nil && ctx.Err() == nil {
				log.Debug("dhcp: obtain failed", "iface", ifname, "err", err)
			}
			return false, nil
		}
		maintCtx, cancel := context.WithCancel(ctx)
		go dhcpc.Maintain(maintCtx, opts, lease)
		return true, cancel
	}
}

// Ethernet manages one ethernet interface: on carrier up it tries DHCP and
// falls back to an IPv4 link-local address; on carrier down it keeps the
// address and only stops DHCP. The carrier signal itself is fed by the caller
// (see linux.go).
type Ethernet struct {
	IfName string
	MAC    net.HardwareAddr
	Ops    netops.Ops
	DHCP   DHCPFunc
	Log    *slog.Logger

	// After and RetryInterval are injectable for tests.
	After         func(time.Duration) <-chan time.Time
	DHCPTimeout   time.Duration // deadline for one DHCP attempt
	RetryInterval time.Duration // how often DHCP is retried while on link-local
	// LinkDownOnCarrierLoss takes the link administratively down on carrier
	// loss. Default false: Linux keeps the interface (and its addresses and
	// TCP connections) usable, and forcing it down on every carrier blip
	// would both kill sockets and drop a lease that is not really gone.
	LinkDownOnCarrierLoss bool

	mu       sync.Mutex
	gen      uint64
	cancel   func()
	stopDHCP func()
}

// NewEthernet returns a configured Ethernet controller using pure-Go netops
// and dhcpc. If ops is nil, netops.Default() is used.
func NewEthernet(ifname string, mac net.HardwareAddr, ops netops.Ops, log *slog.Logger) *Ethernet {
	if log == nil {
		log = slog.Default()
	}
	if ops == nil {
		ops = netops.Default()
	}
	return &Ethernet{
		IfName:        ifname,
		MAC:           mac,
		Ops:           ops,
		DHCP:          defaultDHCPFunc(ops, dhcpc.DefaultResolvConf, log),
		Log:           log,
		After:         time.After,
		DHCPTimeout:   15 * time.Second,
		RetryInterval: 60 * time.Second,
	}
}

func (e *Ethernet) ops() netops.Ops {
	if e.Ops != nil {
		return e.Ops
	}
	return netops.Default()
}

func (e *Ethernet) dhcpFn() DHCPFunc {
	if e.DHCP != nil {
		return e.DHCP
	}
	return defaultDHCPFunc(e.ops(), dhcpc.DefaultResolvConf, e.Log)
}

func (e *Ethernet) after(d time.Duration) <-chan time.Time {
	if e.After != nil {
		return e.After(d)
	}
	return time.After(d)
}

// Up handles a carrier-up event. It returns once the interface is up and a
// DHCP lease or a link-local address is in place. If link-local was chosen,
// a background loop keeps retrying DHCP and drops the link-local address when
// a lease arrives.
func (e *Ethernet) Up(ctx context.Context) {
	opCtx, opGen := e.beginOp(ctx)

	if err := e.ops().LinkUp(e.IfName); err != nil {
		e.stopOp(opGen)
		e.Log.Error("ethernet: bring link up", "iface", e.IfName, "err", err)
		return
	}
	// Lease application uses AddrReplacePrimary (D16): it never flushes an
	// unchanged address across a rebind, so a carrier blip does not kill live
	// connections.
	if ok, stop := e.tryDHCP(opCtx); ok {
		e.setStopDHCP(opGen, stop)
		e.Log.Info("ethernet: dhcp lease", "iface", e.IfName)
		return
	}
	if opCtx.Err() != nil {
		return
	}

	addr, err := e.addLinkLocal(opCtx)
	if err != nil {
		e.stopOp(opGen)
		e.Log.Error("ethernet: link-local failed", "iface", e.IfName, "err", err)
		return
	}
	e.Log.Info("ethernet: using link-local address", "iface", e.IfName, "addr", addr.String())
	go e.retryDHCP(opCtx, opGen, addr)
}

// Down handles a carrier-down event: it stops any DHCP retry/renewal loop and,
// by default, leaves the interface and its addresses alone, so a carrier blip
// does not tear down established connections.
func (e *Ethernet) Down(_ context.Context) error {
	e.stopAll()
	if !e.LinkDownOnCarrierLoss {
		e.Log.Info("ethernet: carrier down, keeping addresses", "iface", e.IfName)
		return nil
	}
	return e.ops().LinkDown(e.IfName)
}

// Flush is the explicit teardown: it stops DHCP and removes every address,
// for when the interface is handed over to another owner.
func (e *Ethernet) Flush(_ context.Context) error {
	e.stopAll()
	return e.ops().AddrFlush(e.IfName)
}

func (e *Ethernet) beginOp(ctx context.Context) (context.Context, uint64) {
	opCtx, cancel := context.WithCancel(ctx)
	e.mu.Lock()
	e.gen++
	gen := e.gen
	prevCancel := e.cancel
	prevStop := e.stopDHCP
	e.cancel = cancel
	e.stopDHCP = nil
	e.mu.Unlock()
	if prevCancel != nil {
		prevCancel()
	}
	if prevStop != nil {
		prevStop()
	}
	return opCtx, gen
}

func (e *Ethernet) setStopDHCP(gen uint64, stop func()) {
	if stop == nil {
		return
	}
	e.mu.Lock()
	if e.gen != gen {
		e.mu.Unlock()
		stop()
		return
	}
	prev := e.stopDHCP
	e.stopDHCP = stop
	e.mu.Unlock()
	if prev != nil {
		prev()
	}
}

func (e *Ethernet) stopOp(gen uint64) {
	e.mu.Lock()
	if e.gen != gen {
		e.mu.Unlock()
		return
	}
	c := e.cancel
	s := e.stopDHCP
	e.cancel = nil
	e.stopDHCP = nil
	e.mu.Unlock()
	if c != nil {
		c()
	}
	if s != nil {
		s()
	}
}

func (e *Ethernet) stopAll() {
	e.mu.Lock()
	e.gen++
	c := e.cancel
	s := e.stopDHCP
	e.cancel = nil
	e.stopDHCP = nil
	e.mu.Unlock()
	if c != nil {
		c()
	}
	if s != nil {
		s()
	}
}

// tryDHCP runs one bounded DHCP attempt and reports whether a lease was
// obtained alongside any background renewal stop function.
func (e *Ethernet) tryDHCP(ctx context.Context) (bool, func()) {
	return e.dhcpFn()(ctx, e.IfName, e.MAC, e.DHCPTimeout)
}

// addLinkLocal picks a free 169.254/16 address with RFC 5227 ARP probing.
func (e *Ethernet) addLinkLocal(ctx context.Context) (net.IP, error) {
	ops := e.ops()
	for attempt := 0; attempt < 10; attempt++ {
		addr := linkLocalIP(e.MAC, attempt)
		inUse, err := ops.ARPProbe(ctx, e.IfName, e.MAC, addr)
		if err != nil {
			if ctx.Err() != nil {
				return nil, ctx.Err()
			}
			e.Log.Warn("ethernet: ARP probe failed, assuming address free", "iface", e.IfName, "addr", addr.String(), "err", err)
		} else if inUse {
			continue
		}
		if err := ops.AddrAdd(e.IfName, addr, 16); err != nil {
			return nil, err
		}
		return addr, nil
	}
	return nil, fmt.Errorf("no free link-local address after 10 attempts")
}

// retryDHCP runs until a DHCP lease is obtained (then the link-local address
// is removed) or the context is cancelled.
func (e *Ethernet) retryDHCP(ctx context.Context, opGen uint64, linkIP net.IP) {
	timer := e.after(e.RetryInterval)
	for {
		select {
		case <-ctx.Done():
			return
		case <-timer:
			if ctx.Err() != nil {
				return // cancelled while the timer was firing
			}
			if ok, stop := e.tryDHCP(ctx); ok {
				e.setStopDHCP(opGen, stop)
				if ctx.Err() != nil {
					return
				}
				if err := e.ops().AddrDel(e.IfName, linkIP, 16); err != nil {
					e.Log.Error("ethernet: drop link-local address", "iface", e.IfName, "err", err)
					return
				}
				e.Log.Info("ethernet: dhcp lease obtained, link-local dropped", "iface", e.IfName)
				return
			}
			timer = e.after(e.RetryInterval)
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
