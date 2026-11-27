package apsrv

import (
	"bytes"
	"encoding/binary"
	"errors"
	"fmt"
	"log/slog"
	"net"
	"sync"
	"time"
)

const (
	dhcpClientPort = 68
	dhcpServerPort = 67

	dhcpOpRequest = 1
	dhcpOpReply   = 2

	dhcpHeaderLen     = 236
	dhcpMinPacketLen  = dhcpHeaderLen + 4 // fixed header + magic cookie
	dhcpMinReplyLen   = 300               // minimum BOOTP reply size for compatibility
	dhcpBufSize       = 2048
	dhcpFlagBroadcast = 0x8000

	// DHCP message types (Option 53).
	dhcpDiscover byte = 1
	dhcpOffer    byte = 2
	dhcpRequest  byte = 3
	dhcpDecline  byte = 4
	dhcpAck      byte = 5
	dhcpNak      byte = 6
	dhcpRelease  byte = 7
	dhcpInform   byte = 8

	// DHCP option codes (RFC 2132 + RFC 8910).
	optPad           byte = 0
	optSubnetMask    byte = 1
	optRouter        byte = 3
	optDNS           byte = 6
	optRequestedIP   byte = 50
	optLeaseTime     byte = 51
	optMessageType   byte = 53
	optServerID      byte = 54
	optCaptivePortal byte = 114
	optEnd           byte = 255

	offerHoldDuration = time.Minute
)

var dhcpMagicCookie = [4]byte{99, 130, 83, 99}

// DHCPReplyAddrFunc computes the UDP destination for a DHCP reply.
// When nil, DHCPServer uses RFC 2131 Section 4.1 routing: broadcast to
// 255.255.255.255:68, or unicast to ciaddr:68 when renewing without the
// broadcast bit set.
type DHCPReplyAddrFunc func(src net.Addr, ciaddr net.IP, broadcast bool, msgType byte) net.Addr

// DHCPConfig configures a DHCPServer.
type DHCPConfig struct {
	ServerIP   net.IP
	Mask       net.IPMask
	PoolStart  net.IP
	PoolEnd    net.IP
	Lease      time.Duration
	CaptiveURL string
	Log        *slog.Logger
	ReplyAddr  DHCPReplyAddrFunc
}

type leaseEntry struct {
	ip      uint32
	expires time.Time
}

// DHCPServer is a minimal thread-safe RFC 2131 DHCPv4 server with an in-memory
// MAC-to-IP lease table and RFC 8910 Captive Portal URI option support.
type DHCPServer struct {
	serverIP   [4]byte
	serverU32  uint32
	mask       [4]byte
	poolStart  uint32
	poolEnd    uint32
	leaseDur   time.Duration
	captiveURL string
	log        *slog.Logger
	replyAddr  DHCPReplyAddrFunc
	now        func() time.Time

	mu      sync.Mutex
	leases  map[string]*leaseEntry // client MAC key -> lease
	ipToMAC map[uint32]string      // IPv4 uint32 -> client MAC key
}

// NewDHCPServer creates a configured DHCPv4 server.
func NewDHCPServer(cfg DHCPConfig) (*DHCPServer, error) {
	ip4 := cfg.ServerIP.To4()
	if ip4 == nil {
		return nil, fmt.Errorf("apsrv: dhcp server IP %v is not IPv4", cfg.ServerIP)
	}
	if len(cfg.Mask) != 4 {
		return nil, fmt.Errorf("apsrv: dhcp subnet mask %v is not 4-byte IPv4", cfg.Mask)
	}
	pStart := cfg.PoolStart.To4()
	pEnd := cfg.PoolEnd.To4()
	if pStart == nil || pEnd == nil {
		return nil, fmt.Errorf("apsrv: dhcp pool bounds %v..%v must be IPv4", cfg.PoolStart, cfg.PoolEnd)
	}
	startU32 := ipToUint32(pStart)
	endU32 := ipToUint32(pEnd)
	if startU32 > endU32 {
		return nil, fmt.Errorf("apsrv: dhcp pool start %s is after end %s", pStart, pEnd)
	}
	lease := cfg.Lease
	if lease <= 0 {
		lease = DefaultLease
	}
	if lease < time.Second {
		return nil, fmt.Errorf("apsrv: dhcp lease %v must be at least 1s", lease)
	}
	captiveURL := cfg.CaptiveURL
	if len(captiveURL) > 255 {
		return nil, fmt.Errorf("apsrv: captive portal URL exceeds 255 bytes (%d)", len(captiveURL))
	}
	log := cfg.Log
	if log == nil {
		log = slog.Default()
	}
	replyFn := cfg.ReplyAddr
	if replyFn == nil {
		replyFn = defaultDHCPReplyAddr
	}

	s := &DHCPServer{
		serverU32:  ipToUint32(ip4),
		poolStart:  startU32,
		poolEnd:    endU32,
		leaseDur:   lease,
		captiveURL: captiveURL,
		log:        log,
		replyAddr:  replyFn,
		now:        time.Now,
		leases:     make(map[string]*leaseEntry),
		ipToMAC:    make(map[uint32]string),
	}
	copy(s.serverIP[:], ip4)
	copy(s.mask[:], cfg.Mask)
	return s, nil
}

// defaultDHCPReplyAddr implements RFC 2131 Section 4.1 reply addressing for a
// local broadcast segment (no relay agent).
func defaultDHCPReplyAddr(_ net.Addr, ciaddr net.IP, broadcast bool, msgType byte) net.Addr {
	if msgType != dhcpNak && ciaddr != nil && !ciaddr.IsUnspecified() && !broadcast {
		return &net.UDPAddr{IP: ciaddr, Port: dhcpClientPort}
	}
	return &net.UDPAddr{IP: net.IPv4bcast, Port: dhcpClientPort}
}

type dhcpReply struct {
	pkt       []byte
	ciaddr    net.IP
	broadcast bool
	msgType   byte
}

// HandlePacket parses a single DHCPv4 request packet, updates lease state, and
// returns the wire reply if one should be transmitted.
func (s *DHCPServer) HandlePacket(req []byte) (*dhcpReply, bool) {
	if len(req) < dhcpMinPacketLen {
		return nil, false
	}
	if req[0] != dhcpOpRequest {
		return nil, false
	}
	hlen := int(req[2])
	if hlen == 0 || hlen > 16 {
		return nil, false
	}
	if !bytes.Equal(req[236:240], dhcpMagicCookie[:]) {
		return nil, false
	}

	opts, ok := parseDHCPOptions(req[240:])
	if !ok || opts.msgType == 0 {
		return nil, false
	}

	// If client addressed a different DHCP server via Option 54, ignore.
	if opts.hasServerID && opts.serverID != s.serverIP {
		return nil, false
	}

	macKey := string(req[28 : 28+hlen])
	flags := binary.BigEndian.Uint16(req[10:12])
	broadcast := flags&dhcpFlagBroadcast != 0
	ciaddr := net.IPv4(req[12], req[13], req[14], req[15]).To4()
	ciaddrU32 := ipToUint32(ciaddr)

	switch opts.msgType {
	case dhcpDiscover:
		ip, allocated := s.allocateOffer(macKey, opts.hasReqIP, opts.reqIP)
		if !allocated {
			s.log.Warn("apsrv: dhcp pool exhausted", "mac", net.HardwareAddr(req[28:28+hlen]).String())
			return nil, false
		}
		pkt := s.buildReply(req, dhcpOffer, ip, false, true)
		return &dhcpReply{
			pkt:       pkt,
			ciaddr:    net.IPv4zero,
			broadcast: broadcast,
			msgType:   dhcpOffer,
		}, true

	case dhcpRequest:
		var targetU32 uint32
		switch {
		case opts.hasReqIP:
			targetU32 = ipToUint32(opts.reqIP[:])
		case ciaddrU32 != 0:
			targetU32 = ciaddrU32
		default:
			pkt := s.buildNak(req)
			return &dhcpReply{
				pkt:       pkt,
				ciaddr:    net.IPv4zero,
				broadcast: true,
				msgType:   dhcpNak,
			}, true
		}

		if !s.confirmLease(macKey, targetU32) {
			pkt := s.buildNak(req)
			return &dhcpReply{
				pkt:       pkt,
				ciaddr:    net.IPv4zero,
				broadcast: true,
				msgType:   dhcpNak,
			}, true
		}
		pkt := s.buildReply(req, dhcpAck, targetU32, true, true)
		return &dhcpReply{
			pkt:       pkt,
			ciaddr:    ciaddr,
			broadcast: broadcast,
			msgType:   dhcpAck,
		}, true

	case dhcpRelease, dhcpDecline:
		s.releaseLease(macKey)
		return nil, false

	case dhcpInform:
		pkt := s.buildReply(req, dhcpAck, 0, true, false)
		return &dhcpReply{
			pkt:       pkt,
			ciaddr:    ciaddr,
			broadcast: broadcast,
			msgType:   dhcpAck,
		}, true

	default:
		return nil, false
	}
}

// Serve reads DHCP packets from conn and transmits replies until conn closes.
func (s *DHCPServer) Serve(conn net.PacketConn) error {
	buf := make([]byte, dhcpBufSize)
	for {
		n, src, err := conn.ReadFrom(buf)
		if err != nil {
			if errors.Is(err, net.ErrClosed) {
				return nil
			}
			return err
		}
		reply, ok := s.HandlePacket(buf[:n])
		if !ok {
			continue
		}
		dst := s.replyAddr(src, reply.ciaddr, reply.broadcast, reply.msgType)
		if dst == nil {
			continue
		}
		if _, err := conn.WriteTo(reply.pkt, dst); err != nil {
			if errors.Is(err, net.ErrClosed) {
				return nil
			}
			s.log.Debug("apsrv: dhcp write failed", "dst", dst, "err", err)
		}
	}
}

// Leases returns a snapshot of currently active MAC-to-IP leases.
func (s *DHCPServer) Leases() map[string]net.IP {
	s.mu.Lock()
	defer s.mu.Unlock()
	now := s.now()
	out := make(map[string]net.IP, len(s.leases))
	for mac, l := range s.leases {
		if now.Before(l.expires) {
			out[net.HardwareAddr(mac).String()] = uint32ToIP(l.ip)
		}
	}
	return out
}

func (s *DHCPServer) inPool(ip uint32) bool {
	return ip >= s.poolStart && ip <= s.poolEnd && ip != s.serverU32
}

func (s *DHCPServer) allocateOffer(mac string, hasReqIP bool, reqIP [4]byte) (uint32, bool) {
	s.mu.Lock()
	defer s.mu.Unlock()

	now := s.now()
	hold := offerHoldDuration
	if s.leaseDur < hold {
		hold = s.leaseDur
	}

	// Reuse existing lease for this MAC if it is still within the pool.
	if existing, ok := s.leases[mac]; ok && s.inPool(existing.ip) {
		if exp := now.Add(hold); existing.expires.Before(exp) {
			existing.expires = exp
		}
		return existing.ip, true
	}

	// Honor client's requested IP if it is in the pool and free/expired.
	if hasReqIP {
		cand := ipToUint32(reqIP[:])
		if s.inPool(cand) && s.isFreeOrExpiredLocked(cand, mac, now) {
			s.assignLocked(mac, cand, now.Add(hold))
			return cand, true
		}
	}

	// First pass: find an unallocated IP in [poolStart, poolEnd].
	for ip := s.poolStart; ip <= s.poolEnd; ip++ {
		if ip == s.serverU32 {
			continue
		}
		if _, used := s.ipToMAC[ip]; !used {
			s.assignLocked(mac, ip, now.Add(hold))
			return ip, true
		}
		if ip == ^uint32(0) {
			break
		}
	}

	// Second pass: reclaim the oldest expired lease.
	var reclaimIP uint32
	var oldest time.Time
	found := false
	for ip, owner := range s.ipToMAC {
		l := s.leases[owner]
		if l == nil || !now.Before(l.expires) {
			if !found || (l != nil && l.expires.Before(oldest)) {
				reclaimIP = ip
				if l != nil {
					oldest = l.expires
				}
				found = true
			}
		}
	}
	if !found {
		return 0, false
	}
	s.assignLocked(mac, reclaimIP, now.Add(hold))
	return reclaimIP, true
}

func (s *DHCPServer) confirmLease(mac string, ip uint32) bool {
	s.mu.Lock()
	defer s.mu.Unlock()

	if !s.inPool(ip) {
		return false
	}
	now := s.now()
	if !s.isFreeOrExpiredLocked(ip, mac, now) {
		return false
	}
	s.assignLocked(mac, ip, now.Add(s.leaseDur))
	return true
}

func (s *DHCPServer) releaseLease(mac string) {
	s.mu.Lock()
	defer s.mu.Unlock()

	l, ok := s.leases[mac]
	if !ok {
		return
	}
	if s.ipToMAC[l.ip] == mac {
		delete(s.ipToMAC, l.ip)
	}
	delete(s.leases, mac)
}

func (s *DHCPServer) isFreeOrExpiredLocked(ip uint32, mac string, now time.Time) bool {
	owner, used := s.ipToMAC[ip]
	if !used || owner == mac {
		return true
	}
	l := s.leases[owner]
	return l == nil || !now.Before(l.expires)
}

func (s *DHCPServer) assignLocked(mac string, ip uint32, expires time.Time) {
	// Detach any previous owner of ip.
	if prevOwner, ok := s.ipToMAC[ip]; ok && prevOwner != mac {
		if prevLease := s.leases[prevOwner]; prevLease != nil && prevLease.ip == ip {
			delete(s.leases, prevOwner)
		}
	}
	// Detach any previous IP held by mac.
	if prev, ok := s.leases[mac]; ok && prev.ip != ip {
		if s.ipToMAC[prev.ip] == mac {
			delete(s.ipToMAC, prev.ip)
		}
	}
	s.leases[mac] = &leaseEntry{ip: ip, expires: expires}
	s.ipToMAC[ip] = mac
}

func (s *DHCPServer) buildReply(req []byte, msgType byte, yiaddr uint32, copyCiaddr, includeLease bool) []byte {
	buf := make([]byte, dhcpMinPacketLen, dhcpMinReplyLen)
	buf[0] = dhcpOpReply
	buf[1] = req[1] // htype
	buf[2] = req[2] // hlen
	buf[3] = 0      // hops
	copy(buf[4:8], req[4:8])
	copy(buf[10:12], req[10:12]) // flags
	if copyCiaddr {
		copy(buf[12:16], req[12:16]) // ciaddr
	}
	if yiaddr != 0 {
		binary.BigEndian.PutUint32(buf[16:20], yiaddr)
	}
	copy(buf[24:28], req[24:28]) // giaddr
	copy(buf[28:44], req[28:44]) // chaddr
	copy(buf[236:240], dhcpMagicCookie[:])

	// Options.
	buf = append(buf, optMessageType, 1, msgType)
	buf = append(buf, optServerID, 4)
	buf = append(buf, s.serverIP[:]...)
	if includeLease {
		buf = append(buf, optLeaseTime, 4)
		buf = binary.BigEndian.AppendUint32(buf, uint32(s.leaseDur/time.Second))
	}
	buf = append(buf, optSubnetMask, 4)
	buf = append(buf, s.mask[:]...)
	buf = append(buf, optRouter, 4)
	buf = append(buf, s.serverIP[:]...)
	buf = append(buf, optDNS, 4)
	buf = append(buf, s.serverIP[:]...)
	if s.captiveURL != "" {
		buf = append(buf, optCaptivePortal, byte(len(s.captiveURL)))
		buf = append(buf, s.captiveURL...)
	}
	buf = append(buf, optEnd)

	if len(buf) < dhcpMinReplyLen {
		buf = append(buf, make([]byte, dhcpMinReplyLen-len(buf))...)
	}
	return buf
}

func (s *DHCPServer) buildNak(req []byte) []byte {
	buf := make([]byte, dhcpMinPacketLen, dhcpMinReplyLen)
	buf[0] = dhcpOpReply
	buf[1] = req[1] // htype
	buf[2] = req[2] // hlen
	copy(buf[4:8], req[4:8])
	copy(buf[10:12], req[10:12]) // flags
	copy(buf[24:28], req[24:28]) // giaddr
	copy(buf[28:44], req[28:44]) // chaddr
	copy(buf[236:240], dhcpMagicCookie[:])

	buf = append(buf, optMessageType, 1, dhcpNak)
	buf = append(buf, optServerID, 4)
	buf = append(buf, s.serverIP[:]...)
	buf = append(buf, optEnd)

	if len(buf) < dhcpMinReplyLen {
		buf = append(buf, make([]byte, dhcpMinReplyLen-len(buf))...)
	}
	return buf
}

type parsedDHCPOptions struct {
	msgType     byte
	hasReqIP    bool
	reqIP       [4]byte
	hasServerID bool
	serverID    [4]byte
}

func parseDHCPOptions(data []byte) (parsedDHCPOptions, bool) {
	var out parsedDHCPOptions
	for i := 0; i < len(data); {
		code := data[i]
		if code == optPad {
			i++
			continue
		}
		if code == optEnd {
			return out, true
		}
		if i+1 >= len(data) {
			return out, false
		}
		n := int(data[i+1])
		if i+2+n > len(data) {
			return out, false
		}
		val := data[i+2 : i+2+n]
		switch code {
		case optMessageType:
			if n == 1 {
				out.msgType = val[0]
			}
		case optRequestedIP:
			if n == 4 {
				copy(out.reqIP[:], val)
				out.hasReqIP = true
			}
		case optServerID:
			if n == 4 {
				copy(out.serverID[:], val)
				out.hasServerID = true
			}
		}
		i += 2 + n
	}
	return out, true
}

func ipToUint32(ip net.IP) uint32 {
	return binary.BigEndian.Uint32(ip[:4])
}

func uint32ToIP(v uint32) net.IP {
	return net.IPv4(byte(v>>24), byte(v>>16), byte(v>>8), byte(v))
}
