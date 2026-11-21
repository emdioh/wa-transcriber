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
// a new or changed address is announced twice, one second apart, and an
// interface that appeared later (Wi-Fi joining) is joined then.
//
// The socket is bound to the wildcard address and joins the multicast group
// per interface, like avahi does. Binding the group address instead (as
// net.ListenMulticastUDP does) has two problems: it cannot receive the
// unicast queries of RFC 6762 section 6.7 that some resolvers send straight to
// the device, and it joins the group itself, so the explicit per-interface
// join fails with EADDRINUSE.
func Listen(ctx context.Context, r *Responder, ifaces func() []Iface, log *slog.Logger) error {
	if log == nil {
		log = slog.Default()
	}
	gaddr, err := net.ResolveUDPAddr("udp4", group)
	if err != nil {
		return err
	}
	conn, err := net.ListenUDP("udp4", &net.UDPAddr{IP: net.IPv4zero, Port: 5353})
	if err != nil {
		return err
	}
	defer conn.Close()
	pc := ipv4.NewPacketConn(conn)
	if err := pc.SetControlMessage(ipv4.FlagInterface|ipv4.FlagDst, true); err != nil {
		return err
	}
	if err := pc.SetMulticastLoopback(true); err != nil {
		log.Warn("mdns: enabling multicast loopback", "err", err)
	}

	joined := map[int]bool{}
	join := func() {
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
	}
	join()

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
				join()
				announceChanged()
				continue
			}
			return err
		}
		pkt := append([]byte(nil), buf[:n]...)
		if ip, ok := r.OtherAddress(pkt); ok {
			// Only a *different* address is a conflict. Our own
			// announcements come back on this socket (multicast
			// loopback) and mDNS reflectors echo them around the LAN;
			// treating those as a conflict renamed the device to
			// wa-2.local on its own address, after which nothing
			// resolved wa.local any more.
			if !isOwnAddress(ip, ifaces()) {
				old := r.Label()
				name := r.Rename()
				log.Warn("mdns: name conflict, renamed", "old", old+".local", "new", name+".local", "ip", ip)
				announceChanged()
			}
			continue
		}
		localIP := localIPFor(ifaces(), cm, src)
		if localIP == nil {
			continue
		}
		// A source port other than 5353 means a one-shot ("simple")
		// resolver: RFC 6762 section 6.7 wants a conventional unicast
		// response, and Android's resolver is exactly that.
		legacy := isLegacySource(src)
		resp, err := r.AnswerTo(pkt, localIP, legacy)
		if err != nil || resp == nil {
			continue
		}
		// A response to a unicast query goes straight back to the
		// querier (RFC 6762 section 6.7); otherwise only the QU bit asks
		// for unicast.
		dst := gaddr
		if resp.Unicast || !isMulticast(cm, gaddr) {
			if ua, ok := src.(*net.UDPAddr); ok {
				dst = ua
			}
		}
		if _, err := pc.WriteTo(resp.Packet, nil, dst); err != nil {
			log.Warn("mdns: write", "err", err)
		}
	}
}

// isOwnAddress reports whether ip is one of the addresses the responder
// serves: a record with it is our own announcement coming back, not a
// conflicting device.
func isOwnAddress(ip net.IP, ifaces []Iface) bool {
	for _, i := range ifaces {
		if i.IP.Equal(ip) {
			return true
		}
	}
	return false
}

// isMulticast reports whether the packet was addressed to the mDNS group (a
// control message is missing the destination only when the kernel did not
// provide it, which happens for some unicast packets).
func isMulticast(cm *ipv4.ControlMessage, gaddr *net.UDPAddr) bool {
	if cm == nil || cm.Dst == nil {
		return false
	}
	return cm.Dst.Equal(gaddr.IP)
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

// localIPFor picks the address to answer with: the destination address of the
// query when the kernel reports one (that is the address the querier used),
// else the address of the interface the query arrived on, else the only
// address there is. Unicast queries arrive without the multicast control
// message on some systems, and dropping them made the device invisible to
// resolvers that ask it directly.
// isLegacySource reports whether the query came from a source port other than
// 5353, i.e. from a one-shot resolver that expects a conventional unicast
// response (RFC 6762 section 6.7).
func isLegacySource(src net.Addr) bool {
	if ua, ok := src.(*net.UDPAddr); ok {
		return ua.Port != 5353
	}
	return false
}

func localIPFor(ifaces []Iface, cm *ipv4.ControlMessage, src net.Addr) net.IP {
	if cm != nil {
		if cm.Dst != nil {
			for _, i := range ifaces {
				if i.IP.Equal(cm.Dst) {
					return i.IP
				}
			}
		}
		for _, i := range ifaces {
			if cm.IfIndex != 0 && i.Index == cm.IfIndex {
				return i.IP
			}
		}
	}
	if len(ifaces) == 1 {
		return ifaces[0].IP
	}
	// Same link as the querier: the first address that shares the /24 the
	// query came from. Only a fallback for when the kernel reports nothing.
	if ua, ok := src.(*net.UDPAddr); ok && ua.IP.To4() != nil {
		for _, i := range ifaces {
			if i.IP.To4()[0] == ua.IP.To4()[0] &&
				i.IP.To4()[1] == ua.IP.To4()[1] &&
				i.IP.To4()[2] == ua.IP.To4()[2] {
				return i.IP
			}
		}
	}
	return nil
}
