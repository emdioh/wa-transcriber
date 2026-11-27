package dhcpc

import (
	"bytes"
	"context"
	"crypto/rand"
	"encoding/binary"
	"errors"
	"fmt"
	"log/slog"
	"net"
	"os"
	"path/filepath"
	"time"
)

const (
	// DefaultTimeout is the default outer deadline for Obtain.
	DefaultTimeout = 15 * time.Second
	// DefaultRetryInterval is the default delay between retransmitted DISCOVER/REQUEST packets.
	DefaultRetryInterval = 3 * time.Second
	// DefaultResolvConf is the default path updated by WriteResolvConf.
	DefaultResolvConf = "/etc/resolv.conf"
)

// Transport sends and receives raw DHCPv4 UDP packets.
type Transport interface {
	// Send transmits a marshalled DHCP packet. When dst is nil the transport
	// uses its default destination (255.255.255.255:67 on Linux).
	Send(pkt []byte, dst *net.UDPAddr) error
	// Recv reads the next packet before deadline, returning a net.Error with
	// Timeout()==true when deadline expires.
	Recv(deadline time.Time) ([]byte, error)
	Close() error
}

// Options configures Obtain and Maintain.
type Options struct {
	IfName        string
	MAC           net.HardwareAddr
	Hostname      string
	Timeout       time.Duration
	RetryInterval time.Duration
	ResolvConf    string
	Apply         func(Lease) error
	Log           *slog.Logger

	// Dial optionally overrides the default Linux UDP4 broadcast transport.
	// Used by tests to talk to a loopback UDP server without root privileges.
	Dial func(ctx context.Context) (Transport, error)
}

type udpTransport struct {
	conn       *net.UDPConn
	defaultDst *net.UDPAddr
}

// NewUDPTransport wraps conn as a Transport whose nil-destination sends go to
// defaultDst.
func NewUDPTransport(conn *net.UDPConn, defaultDst *net.UDPAddr) Transport {
	return &udpTransport{conn: conn, defaultDst: defaultDst}
}

func (t *udpTransport) Send(pkt []byte, dst *net.UDPAddr) error {
	if dst == nil {
		dst = t.defaultDst
	}
	_, err := t.conn.WriteToUDP(pkt, dst)
	return err
}

func (t *udpTransport) Recv(deadline time.Time) ([]byte, error) {
	if err := t.conn.SetReadDeadline(deadline); err != nil {
		return nil, err
	}
	buf := make([]byte, 2048)
	n, _, err := t.conn.ReadFromUDP(buf)
	if err != nil {
		return nil, err
	}
	return buf[:n], nil
}

func (t *udpTransport) Close() error {
	return t.conn.Close()
}

func (o Options) withDefaults() (Options, error) {
	if o.Timeout <= 0 {
		o.Timeout = DefaultTimeout
	}
	if o.RetryInterval <= 0 {
		o.RetryInterval = DefaultRetryInterval
	}
	if o.Log == nil {
		o.Log = slog.Default()
	}
	if len(o.MAC) == 0 && o.IfName != "" {
		ifi, err := net.InterfaceByName(o.IfName)
		if err != nil {
			return o, fmt.Errorf("dhcpc: lookup interface %q: %w", o.IfName, err)
		}
		o.MAC = cloneBytes(ifi.HardwareAddr)
	}
	if len(o.MAC) == 0 {
		return o, errors.New("dhcpc: MAC address is required")
	}
	return o, nil
}

func (o Options) dial(ctx context.Context) (Transport, error) {
	if o.Dial != nil {
		return o.Dial(ctx)
	}
	return dialDefault(ctx, o.IfName)
}

func (o Options) resolvConfPath() string {
	if o.ResolvConf != "" {
		return o.ResolvConf
	}
	// Only write /etc/resolv.conf by default when using the real transport;
	// tests that inject Dial do not touch the host's /etc/resolv.conf unless
	// they explicitly configure ResolvConf.
	if o.Dial == nil {
		return DefaultResolvConf
	}
	return ""
}

// Obtain runs the RFC 2131 DISCOVER -> OFFER -> REQUEST -> ACK handshake until
// a lease is obtained or ctx / opts.Timeout expires. It never drops existing
// interface addresses on failure or retry (D16). When opts.Apply is non-nil it
// is invoked with the acquired lease before returning.
func Obtain(ctx context.Context, opts Options) (Lease, error) {
	opts, err := opts.withDefaults()
	if err != nil {
		return Lease{}, err
	}

	ctx, cancel := context.WithTimeout(ctx, opts.Timeout)
	defer cancel()

	tr, err := opts.dial(ctx)
	if err != nil {
		return Lease{}, err
	}
	defer tr.Close()

	stop := context.AfterFunc(ctx, func() {
		_ = tr.Close()
	})
	defer stop()

	xid := randomXID()
	for {
		if err := ctx.Err(); err != nil {
			return Lease{}, err
		}

		discover := NewDiscover(xid, opts.MAC, opts.Hostname)
		rawDiscover, err := discover.Marshal()
		if err != nil {
			return Lease{}, err
		}
		if err := tr.Send(rawDiscover, nil); err != nil {
			if ctx.Err() != nil {
				return Lease{}, ctx.Err()
			}
			opts.Log.Warn("dhcpc: send DISCOVER failed", "iface", opts.IfName, "err", err)
			if !sleepCtx(ctx, opts.RetryInterval) {
				return Lease{}, ctx.Err()
			}
			continue
		}

		offer, err := waitForOffer(ctx, tr, xid, opts.MAC, opts.RetryInterval)
		if err != nil {
			if ctx.Err() != nil {
				return Lease{}, ctx.Err()
			}
			if isTimeout(err) {
				continue
			}
			return Lease{}, err
		}

		serverID := offer.effectiveServerID()
		req := NewRequest(xid, opts.MAC, opts.Hostname, offer.YIAddr, serverID)
		rawReq, err := req.Marshal()
		if err != nil {
			return Lease{}, err
		}

		var ack *Packet
		var nak bool
		for attempt := 0; attempt < 2 && ctx.Err() == nil; attempt++ {
			if err := tr.Send(rawReq, nil); err != nil {
				if ctx.Err() != nil {
					return Lease{}, ctx.Err()
				}
				opts.Log.Warn("dhcpc: send REQUEST failed", "iface", opts.IfName, "err", err)
				break
			}
			pkt, err := waitForAckOrNak(ctx, tr, xid, opts.MAC, opts.RetryInterval)
			if err != nil {
				if ctx.Err() != nil {
					return Lease{}, ctx.Err()
				}
				if isTimeout(err) {
					continue
				}
				return Lease{}, err
			}
			if pkt.MessageType == MsgNak {
				opts.Log.Warn("dhcpc: received NAK, restarting DISCOVER", "iface", opts.IfName, "ip", offer.YIAddr)
				nak = true
				break
			}
			ack = pkt
			break
		}
		if nak || ack == nil {
			xid = randomXID()
			continue
		}

		lease, err := LeaseFromPacket(ack, time.Now())
		if err != nil {
			opts.Log.Warn("dhcpc: ignoring invalid ACK", "iface", opts.IfName, "err", err)
			xid = randomXID()
			continue
		}

		if err := applyLease(opts, lease); err != nil {
			return Lease{}, err
		}
		return lease, nil
	}
}

// Maintain renews initial in the background: it sleeps until T1, attempts
// renewal with a REQUEST carrying ciaddr, falls back to a full Obtain at T2 /
// expiry (or on NAK), and applies updated leases via opts.Apply. It exits
// cleanly on ctx.Done() without releasing the lease (D16).
func Maintain(ctx context.Context, opts Options, initial Lease) {
	normOpts, err := opts.withDefaults()
	if err != nil {
		if opts.Log != nil {
			opts.Log.Error("dhcpc: invalid options for maintain", "err", err)
		}
		return
	}
	opts = normOpts

	lease := normalizeLeaseTimers(initial)

	for ctx.Err() == nil {
		t1At := lease.Obtained.Add(lease.T1)
		t2At := lease.Obtained.Add(lease.T2)

		if !sleepUntil(ctx, t1At) {
			return
		}

		renewed, nak, err := renew(ctx, opts, lease, t2At)
		if err == nil {
			if applyErr := applyLease(opts, renewed); applyErr != nil {
				opts.Log.Warn("dhcpc: apply renewed lease failed", "iface", opts.IfName, "ip", renewed.IP, "err", applyErr)
			}
			lease = normalizeLeaseTimers(renewed)
			continue
		}
		if ctx.Err() != nil {
			return
		}
		if nak {
			opts.Log.Warn("dhcpc: renewal NAK, re-obtaining lease", "iface", opts.IfName, "ip", lease.IP)
		} else {
			opts.Log.Warn("dhcpc: renewal failed before T2, falling back to full obtain", "iface", opts.IfName, "ip", lease.IP, "err", err)
		}

		// Rebinding / expiry fallback: run full Obtain until a lease arrives or
		// ctx is cancelled. Never drop the current lease while retrying (D16).
		for ctx.Err() == nil {
			nextLease, err := Obtain(ctx, opts)
			if err == nil {
				lease = normalizeLeaseTimers(nextLease)
				break
			}
			if ctx.Err() != nil {
				return
			}
			opts.Log.Warn("dhcpc: rebind obtain failed, keeping existing lease", "iface", opts.IfName, "ip", lease.IP, "err", err)
			if !sleepCtx(ctx, opts.RetryInterval) {
				return
			}
		}
	}
}

func renew(ctx context.Context, opts Options, current Lease, deadline time.Time) (Lease, bool, error) {
	if time.Now().After(deadline) {
		return Lease{}, false, context.DeadlineExceeded
	}
	rctx, cancel := context.WithDeadline(ctx, deadline)
	defer cancel()

	tr, err := opts.dial(rctx)
	if err != nil {
		return Lease{}, false, err
	}
	defer tr.Close()

	stop := context.AfterFunc(rctx, func() {
		_ = tr.Close()
	})
	defer stop()

	xid := randomXID()
	var unicastDst *net.UDPAddr
	if ip := current.ServerID.To4(); ip != nil && !ip.Equal(net.IPv4zero) {
		unicastDst = &net.UDPAddr{IP: ip, Port: ServerPort}
	}

	unicastReq, err := NewRenewRequest(xid, opts.MAC, opts.Hostname, current.IP, false).Marshal()
	if err != nil {
		return Lease{}, false, err
	}
	bcastReq, err := NewRenewRequest(xid, opts.MAC, opts.Hostname, current.IP, true).Marshal()
	if err != nil {
		return Lease{}, false, err
	}

	for rctx.Err() == nil {
		var sendErr error
		if unicastDst != nil {
			sendErr = tr.Send(unicastReq, unicastDst)
			if sendErr != nil {
				sendErr = tr.Send(bcastReq, nil)
			}
		} else {
			sendErr = tr.Send(bcastReq, nil)
		}
		if sendErr != nil {
			if rctx.Err() != nil {
				return Lease{}, false, rctx.Err()
			}
			return Lease{}, false, sendErr
		}

		wait := opts.RetryInterval
		if rem := time.Until(deadline); rem < wait {
			wait = rem
		}
		if wait <= 0 {
			return Lease{}, false, context.DeadlineExceeded
		}

		pkt, err := waitForAckOrNak(rctx, tr, xid, opts.MAC, wait)
		if err != nil {
			if rctx.Err() != nil {
				return Lease{}, false, rctx.Err()
			}
			if isTimeout(err) {
				continue
			}
			return Lease{}, false, err
		}
		if pkt.MessageType == MsgNak {
			return Lease{}, true, errors.New("dhcpc: server sent NAK")
		}
		lease, err := LeaseFromPacket(pkt, time.Now())
		if err != nil {
			continue
		}
		return lease, false, nil
	}

	return Lease{}, false, rctx.Err()
}

func waitForOffer(ctx context.Context, tr Transport, xid uint32, mac net.HardwareAddr, wait time.Duration) (*Packet, error) {
	deadline := capDeadline(ctx, time.Now().Add(wait))
	for {
		raw, err := tr.Recv(deadline)
		if err != nil {
			return nil, err
		}
		pkt, err := ParsePacket(raw)
		if err != nil {
			continue
		}
		if !matchesReply(pkt, xid, mac) || pkt.MessageType != MsgOffer {
			continue
		}
		if pkt.YIAddr == nil || pkt.YIAddr.Equal(net.IPv4zero) || pkt.effectiveServerID() == nil {
			continue
		}
		return pkt, nil
	}
}

func waitForAckOrNak(ctx context.Context, tr Transport, xid uint32, mac net.HardwareAddr, wait time.Duration) (*Packet, error) {
	deadline := capDeadline(ctx, time.Now().Add(wait))
	for {
		raw, err := tr.Recv(deadline)
		if err != nil {
			return nil, err
		}
		pkt, err := ParsePacket(raw)
		if err != nil {
			continue
		}
		if !matchesReply(pkt, xid, mac) {
			continue
		}
		if pkt.MessageType == MsgAck || pkt.MessageType == MsgNak {
			return pkt, nil
		}
	}
}

func matchesReply(pkt *Packet, xid uint32, mac net.HardwareAddr) bool {
	return pkt.Op == OpReply && pkt.XID == xid && bytes.Equal(pkt.CHAddr, mac)
}

func applyLease(opts Options, lease Lease) error {
	if opts.Apply != nil {
		if err := opts.Apply(lease); err != nil {
			return err
		}
	}
	if p := opts.resolvConfPath(); p != "" && len(lease.DNS) > 0 {
		if err := WriteResolvConf(p, lease.DNS); err != nil {
			return err
		}
	}
	return nil
}

// WriteResolvConf writes a resolv.conf listing servers, preceded by a header
// comment. If the existing file already has identical contents it is left
// untouched so its mtime is preserved (D16 idempotency). Symlinks such as
// /etc/resolv.conf -> /tmp/resolv.conf are followed and updated atomically
// via a temp file in the resolved target's directory.
func WriteResolvConf(path string, servers []net.IP) error {
	if path == "" {
		path = DefaultResolvConf
	}

	var buf bytes.Buffer
	buf.WriteString("# written by wa-transcriber\n")
	for _, s := range servers {
		if s == nil {
			continue
		}
		if v4 := s.To4(); v4 != nil {
			s = v4
		}
		buf.WriteString("nameserver ")
		buf.WriteString(s.String())
		buf.WriteByte('\n')
	}
	want := buf.Bytes()

	target, err := resolveSymlinkTarget(path)
	if err != nil {
		return fmt.Errorf("dhcpc: resolve %q: %w", path, err)
	}

	if existing, err := os.ReadFile(target); err == nil && bytes.Equal(existing, want) {
		return nil
	}

	dir := filepath.Dir(target)
	tmp, err := os.CreateTemp(dir, ".resolv.conf.*")
	if err != nil {
		return os.WriteFile(target, want, 0o644)
	}
	tmpName := tmp.Name()
	cleanup := true
	defer func() {
		if cleanup {
			_ = os.Remove(tmpName)
		}
	}()

	if _, err := tmp.Write(want); err != nil {
		_ = tmp.Close()
		return err
	}
	if err := tmp.Chmod(0o644); err != nil {
		_ = tmp.Close()
		return err
	}
	if err := tmp.Close(); err != nil {
		return err
	}
	if err := os.Rename(tmpName, target); err != nil {
		return os.WriteFile(target, want, 0o644)
	}
	cleanup = false
	return nil
}

// resolveSymlinkTarget follows symlinks like filepath.EvalSymlinks, but also
// handles dangling final targets (e.g. /etc/resolv.conf -> /tmp/resolv.conf
// on a fresh tmpfs before the target exists).
func resolveSymlinkTarget(path string) (string, error) {
	cur := path
	for range 16 {
		fi, err := os.Lstat(cur)
		if err != nil {
			if errors.Is(err, os.ErrNotExist) {
				return cur, nil
			}
			return "", err
		}
		if fi.Mode()&os.ModeSymlink == 0 {
			return cur, nil
		}
		link, err := os.Readlink(cur)
		if err != nil {
			return "", err
		}
		if !filepath.IsAbs(link) {
			link = filepath.Join(filepath.Dir(cur), link)
		}
		cur = filepath.Clean(link)
	}
	return "", fmt.Errorf("too many symlinks resolving %q", path)
}

func normalizeLeaseTimers(l Lease) Lease {
	if l.Obtained.IsZero() {
		l.Obtained = time.Now()
	}
	if l.Duration <= 0 {
		l.Duration = MinLeaseDuration
	}
	if l.T2 <= 0 || l.T2 >= l.Duration {
		l.T2 = l.Duration * 7 / 8
	}
	if l.T1 <= 0 || l.T1 >= l.T2 {
		l.T1 = l.Duration / 2
		if l.T1 >= l.T2 {
			l.T1 = l.T2 / 2
		}
	}
	return l
}

func randomXID() uint32 {
	var b [4]byte
	if _, err := rand.Read(b[:]); err != nil {
		return uint32(time.Now().UnixNano())
	}
	return binary.BigEndian.Uint32(b[:])
}

func capDeadline(ctx context.Context, d time.Time) time.Time {
	if cd, ok := ctx.Deadline(); ok && cd.Before(d) {
		return cd
	}
	return d
}

func isTimeout(err error) bool {
	var ne net.Error
	return errors.As(err, &ne) && ne.Timeout()
}

func sleepUntil(ctx context.Context, target time.Time) bool {
	d := time.Until(target)
	if d <= 0 {
		return ctx.Err() == nil
	}
	return sleepCtx(ctx, d)
}

func sleepCtx(ctx context.Context, d time.Duration) bool {
	if d <= 0 {
		return ctx.Err() == nil
	}
	t := time.NewTimer(d)
	defer t.Stop()
	select {
	case <-ctx.Done():
		return false
	case <-t.C:
		return true
	}
}
