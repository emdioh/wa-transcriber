package mdns

import (
	"context"
	"log/slog"
	"net"
	"time"

	"golang.org/x/net/ipv4"
)

const (
	group       = "224.0.0.251:5353"
	announceGap = time.Second
)

// Iface is one interface the responder serves.
type Iface struct {
	Index int
	IP    net.IP
	Name  string
}

// Listen runs the responder on every interface returned by ifaces until ctx
// ends. ifaces is called periodically so address changes (DHCP) are picked up.
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

	buf := make([]byte, 9000)
	for {
		n, cm, src, err := pc.ReadFrom(buf)
		if err != nil {
			if ctx.Err() != nil {
				return nil
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

// Announce sends two unsolicited responses one second apart, as PLAN.md M5
// step 9 requires after an address change.
func Announce(ctx context.Context, r *Responder, ifIP net.IP, log *slog.Logger) error {
	gaddr, err := net.ResolveUDPAddr("udp4", group)
	if err != nil {
		return err
	}
	conn, err := net.DialUDP("udp4", nil, gaddr)
	if err != nil {
		return err
	}
	defer conn.Close()
	pkt, err := r.Announce(ifIP)
	if err != nil {
		return err
	}
	if _, err := conn.Write(pkt); err != nil {
		return err
	}
	select {
	case <-ctx.Done():
		return nil
	case <-time.After(announceGap):
	}
	_, err = conn.Write(pkt)
	return err
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
