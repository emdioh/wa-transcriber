package mdns

import (
	"context"
	"errors"
	"log/slog"
	"net"
	"time"

	"golang.org/x/net/ipv4"
)

const (
	group         = "224.0.0.251:5353"
	announceGap   = time.Second     // between the two unsolicited responses
	announceCheck = 2 * time.Second // how often to look for address changes
)

// Iface is one interface the responder serves.
type Iface struct {
	Index int
	IP    net.IP
	Name  string
}

// Listen runs the responder on every interface returned by ifaces until ctx
// ends. ifaces is called periodically so address changes (DHCP) are picked up:
// a new or changed address is announced twice, one second apart.
func Listen(ctx context.Context, r *Responder, ifaces func() []Iface, log *slog.Logger) error {
	if log == nil {
		log = slog.Default()
	}
	gaddr, err := net.ResolveUDPAddr("udp4", group)
	if err != nil {
		return err
	}
	conn, err := net.ListenMulticastUDP("udp4", nil, gaddr)
	if err != nil {
		return err
	}
	defer conn.Close()
	pc := ipv4.NewPacketConn(conn)
	if err := pc.SetControlMessage(ipv4.FlagInterface, true); err != nil {
		return err
	}
	if err := pc.SetMulticastLoopback(true); err != nil {
		log.Warn("mdns: enabling multicast loopback", "err", err)
	}

	joined := map[int]bool{}
	for _, i := range ifaces() {
		if joined[i.Index] {
			continue
		}
		if err := pc.JoinGroup(&net.Interface{Index: i.Index, Name: i.Name}, gaddr); err != nil {
			log.Warn("mdns: join group", "iface", i.Name, "err", err)
			continue
		}
		joined[i.Index] = true
	}

	go func() {
		<-ctx.Done()
		conn.Close()
	}()

	tr := &addrTracker{last: map[int]net.IP{}}
	// announceChanged pushes the unsolicited responses for every interface
	// whose address appeared or changed since the previous call. The two
	// packets are 1 s apart, so they must not block the query loop.
	announceChanged := func() {
		for _, i := range tr.Changed(ifaces()) {
			go func(i Iface) {
				if err := announce(ctx, pc, r, i, gaddr, announceGap); err != nil && ctx.Err() == nil {
					log.Warn("mdns: announce", "iface", i.Name, "err", err)
				}
			}(i)
		}
	}
	announceChanged() // startup: no client has the address cached yet

	buf := make([]byte, 9000)
	for {
		if err := pc.SetReadDeadline(time.Now().Add(announceCheck)); err != nil {
			return err
		}
		n, cm, src, err := pc.ReadFrom(buf)
		if err != nil {
			if ctx.Err() != nil {
				return nil
			}
			var ne net.Error
			if errors.As(err, &ne) && ne.Timeout() {
				announceChanged()
				continue
			}
			return err
		}
		pkt := append([]byte(nil), buf[:n]...)
		if ip, ok := r.OtherAddress(pkt); ok {
			old := r.Label()
			name := r.Rename()
			log.Warn("mdns: name conflict, renamed", "old", old+".local", "new", name+".local", "ip", ip)
			continue
		}
		localIP := localIPFor(ifaces(), cm)
		if localIP == nil {
			continue
		}
		resp, err := r.Answer(pkt, localIP)
		if err != nil || resp == nil {
			continue
		}
		dst := gaddr
		if resp.Unicast {
			if ua, ok := src.(*net.UDPAddr); ok {
				dst = ua
			}
		}
		if _, err := pc.WriteTo(resp.Packet, nil, dst); err != nil {
			log.Warn("mdns: write", "err", err)
		}
	}
}

// sender is the part of an *ipv4.PacketConn needed to emit an announcement.
type sender interface {
	WriteTo(b []byte, cm *ipv4.ControlMessage, dst net.Addr) (n int, err error)
}

// announce sends the two unsolicited responses for one interface out of that
// interface, gap apart, as PLAN.md M5 step 9 requires after an address change.
func announce(ctx context.Context, s sender, r *Responder, i Iface, gaddr net.Addr, gap time.Duration) error {
	pkt, err := r.Announce(i.IP)
	if err != nil {
		return err
	}
	cm := &ipv4.ControlMessage{IfIndex: i.Index}
	for n := 0; ; n++ {
		if _, err := s.WriteTo(pkt, cm, gaddr); err != nil {
			return err
		}
		if n == 1 {
			return nil
		}
		select {
		case <-ctx.Done():
			return nil
		case <-time.After(gap):
		}
	}
}

// addrTracker remembers the last announced address per interface, so a new or
// changed address is announced exactly once.
type addrTracker struct {
	last map[int]net.IP
}

// Changed records ifaces and returns those whose address is new or different
// since the previous call, in the order given. Interfaces that disappeared are
// forgotten, so a later return is announced again.
func (t *addrTracker) Changed(ifaces []Iface) []Iface {
	seen := make(map[int]bool, len(ifaces))
	var out []Iface
	for _, i := range ifaces {
		ip := i.IP.To4()
		if ip == nil {
			continue
		}
		seen[i.Index] = true
		if old, ok := t.last[i.Index]; ok && old.Equal(ip) {
			continue
		}
		t.last[i.Index] = ip
		out = append(out, Iface{Index: i.Index, IP: ip, Name: i.Name})
	}
	for idx := range t.last {
		if !seen[idx] {
			delete(t.last, idx)
		}
	}
	return out
}

func localIPFor(ifaces []Iface, cm *ipv4.ControlMessage) net.IP {
	if cm == nil {
		return nil
	}
	for _, i := range ifaces {
		if i.Index == cm.IfIndex {
			return i.IP
		}
	}
	return nil
}
