package apsrv

import (
	"bytes"
	"context"
	"encoding/binary"
	"net"
	"runtime"
	"strings"
	"testing"
	"time"
)

func startLoopbackServer(t *testing.T, cfg Config) *Server {
	t.Helper()
	if cfg.DHCPAddr == "" {
		cfg.DHCPAddr = "127.0.0.1:0"
	}
	if cfg.DNSAddr == "" {
		cfg.DNSAddr = "127.0.0.1:0"
	}
	if cfg.DHCPReplyAddr == nil {
		cfg.DHCPReplyAddr = func(src net.Addr, _ net.IP, _ bool, _ byte) net.Addr {
			return src
		}
	}
	srv, err := Start(context.Background(), cfg)
	if err != nil {
		t.Fatalf("Start: %v", err)
	}
	t.Cleanup(func() {
		if err := srv.Close(); err != nil {
			t.Errorf("Close: %v", err)
		}
	})
	return srv
}

func buildDNSQuery(id, flags uint16, name string, qtype, qclass uint16, extra []byte) []byte {
	var buf []byte
	buf = binary.BigEndian.AppendUint16(buf, id)
	buf = binary.BigEndian.AppendUint16(buf, flags)
	buf = binary.BigEndian.AppendUint16(buf, 1) // QDCOUNT
	buf = binary.BigEndian.AppendUint16(buf, 0) // ANCOUNT
	buf = binary.BigEndian.AppendUint16(buf, 0) // NSCOUNT
	if len(extra) > 0 {
		buf = binary.BigEndian.AppendUint16(buf, 1) // ARCOUNT
	} else {
		buf = binary.BigEndian.AppendUint16(buf, 0)
	}
	for _, label := range strings.Split(strings.TrimSuffix(name, "."), ".") {
		if label == "" {
			continue
		}
		buf = append(buf, byte(len(label)))
		buf = append(buf, label...)
	}
	buf = append(buf, 0) // root terminator
	buf = binary.BigEndian.AppendUint16(buf, qtype)
	buf = binary.BigEndian.AppendUint16(buf, qclass)
	buf = append(buf, extra...)
	return buf
}

func udpExchange(t *testing.T, addr net.Addr, req []byte) []byte {
	t.Helper()
	conn, err := net.DialTimeout("udp4", addr.String(), time.Second)
	if err != nil {
		t.Fatalf("dial %s: %v", addr, err)
	}
	defer conn.Close()
	if err := conn.SetDeadline(time.Now().Add(2 * time.Second)); err != nil {
		t.Fatalf("SetDeadline: %v", err)
	}
	if _, err := conn.Write(req); err != nil {
		t.Fatalf("Write: %v", err)
	}
	buf := make([]byte, 2048)
	n, err := conn.Read(buf)
	if err != nil {
		t.Fatalf("Read: %v", err)
	}
	return buf[:n]
}

func TestDNSWildcardAAndEmptyOther(t *testing.T) {
	srv := startLoopbackServer(t, Config{})

	// 1. A queries for example.com (RD=1) and captive.apple.com (RD=0).
	for _, tc := range []struct {
		id    uint16
		flags uint16
		name  string
		extra []byte
	}{
		{id: 0x1234, flags: 0x0100, name: "example.com"},
		{id: 0xBEEF, flags: 0x0000, name: "captive.apple.com"},
		{
			// Include a minimal EDNS0 OPT pseudo-RR (root, type 41, payload 1232, ext 0, rdlen 0).
			id:    0x4242,
			flags: 0x0100,
			name:  "connectivitycheck.gstatic.com",
			extra: []byte{0x00, 0x00, 0x29, 0x04, 0xD0, 0x00, 0x00, 0x00, 0x00, 0x00, 0x00},
		},
	} {
		q := buildDNSQuery(tc.id, tc.flags, tc.name, 1, 1, tc.extra)
		qLen := len(q) - len(tc.extra)
		resp := udpExchange(t, srv.DNSAddr(), q)

		if gotID := binary.BigEndian.Uint16(resp[0:2]); gotID != tc.id {
			t.Fatalf("%s: id = %#x, want %#x", tc.name, gotID, tc.id)
		}
		wantFlags := uint16(0x8480) | (tc.flags & 0x0100)
		if gotFlags := binary.BigEndian.Uint16(resp[2:4]); gotFlags != wantFlags {
			t.Fatalf("%s: flags = %#04x, want %#04x", tc.name, gotFlags, wantFlags)
		}
		if qd := binary.BigEndian.Uint16(resp[4:6]); qd != 1 {
			t.Fatalf("%s: QDCOUNT = %d, want 1", tc.name, qd)
		}
		if an := binary.BigEndian.Uint16(resp[6:8]); an != 1 {
			t.Fatalf("%s: ANCOUNT = %d, want 1", tc.name, an)
		}
		if ns := binary.BigEndian.Uint16(resp[8:10]); ns != 0 {
			t.Fatalf("%s: NSCOUNT = %d, want 0", tc.name, ns)
		}
		if ar := binary.BigEndian.Uint16(resp[10:12]); ar != 0 {
			t.Fatalf("%s: ARCOUNT = %d, want 0", tc.name, ar)
		}
		if len(resp) != qLen+16 {
			t.Fatalf("%s: resp len = %d, want %d", tc.name, len(resp), qLen+16)
		}
		if !bytes.Equal(resp[12:qLen], q[12:qLen]) {
			t.Fatalf("%s: echoed question mismatch", tc.name)
		}
		ans := resp[qLen:]
		if ans[0] != 0xC0 || ans[1] != 0x0C {
			t.Fatalf("%s: answer name ptr = %#x %#x, want 0xc0 0x0c", tc.name, ans[0], ans[1])
		}
		if typ := binary.BigEndian.Uint16(ans[2:4]); typ != 1 {
			t.Fatalf("%s: answer type = %d, want 1", tc.name, typ)
		}
		if cls := binary.BigEndian.Uint16(ans[4:6]); cls != 1 {
			t.Fatalf("%s: answer class = %d, want 1", tc.name, cls)
		}
		if ttl := binary.BigEndian.Uint32(ans[6:10]); ttl != DefaultDNSTTL {
			t.Fatalf("%s: answer TTL = %d, want %d", tc.name, ttl, DefaultDNSTTL)
		}
		if rdlen := binary.BigEndian.Uint16(ans[10:12]); rdlen != 4 {
			t.Fatalf("%s: answer RDLENGTH = %d, want 4", tc.name, rdlen)
		}
		if gotIP := net.IP(ans[12:16]); !gotIP.Equal(net.IPv4(192, 168, 4, 1)) {
			t.Fatalf("%s: answer IP = %s, want 192.168.4.1", tc.name, gotIP)
		}
	}

	// 2. Non-A queries (AAAA=28, HTTPS=65, TXT=16) get NOERROR with ANCOUNT=0.
	for _, qtype := range []uint16{28, 65, 16} {
		q := buildDNSQuery(0x55AA, 0x0100, "example.com", qtype, 1, nil)
		resp := udpExchange(t, srv.DNSAddr(), q)
		if len(resp) != len(q) {
			t.Fatalf("qtype %d: resp len = %d, want %d", qtype, len(resp), len(q))
		}
		if flags := binary.BigEndian.Uint16(resp[2:4]); flags != 0x8580 {
			t.Fatalf("qtype %d: flags = %#04x, want 0x8580", qtype, flags)
		}
		if an := binary.BigEndian.Uint16(resp[6:8]); an != 0 {
			t.Fatalf("qtype %d: ANCOUNT = %d, want 0", qtype, an)
		}
		if !bytes.Equal(resp[12:], q[12:]) {
			t.Fatalf("qtype %d: echoed question mismatch", qtype)
		}
	}

	// 3. Malformed packets must not crash the server; subsequent valid query works.
	malformed := [][]byte{
		{},
		{0x01, 0x02, 0x03},
		// QR=1 (already a response)
		{0x00, 0x01, 0x80, 0x00, 0x00, 0x01, 0x00, 0x00, 0x00, 0x00, 0x00, 0x00, 0x00, 0x00, 0x01, 0x00, 0x01},
		// QDCOUNT=0
		{0x00, 0x01, 0x01, 0x00, 0x00, 0x00, 0x00, 0x00, 0x00, 0x00, 0x00, 0x00},
		// Truncated label (claims length 20, only 3 bytes remain)
		{0x00, 0x01, 0x01, 0x00, 0x00, 0x01, 0x00, 0x00, 0x00, 0x00, 0x00, 0x00, 0x14, 'a', 'b', 'c'},
		// Compression pointer in question
		{0x00, 0x01, 0x01, 0x00, 0x00, 0x01, 0x00, 0x00, 0x00, 0x00, 0x00, 0x00, 0xC0, 0x0C, 0x00, 0x01, 0x00, 0x01},
		// Missing QTYPE/QCLASS after root label
		{0x00, 0x01, 0x01, 0x00, 0x00, 0x01, 0x00, 0x00, 0x00, 0x00, 0x00, 0x00, 0x00, 0x00, 0x01},
	}
	conn, err := net.DialTimeout("udp4", srv.DNSAddr().String(), time.Second)
	if err != nil {
		t.Fatalf("dial dns: %v", err)
	}
	defer conn.Close()
	for _, pkt := range malformed {
		if len(pkt) == 0 {
			if resp, ok := srv.DNS.HandleQuery(pkt); ok || resp != nil {
				t.Fatalf("empty query unexpectedly answered")
			}
			continue
		}
		if _, err := conn.Write(pkt); err != nil {
			t.Fatalf("write malformed: %v", err)
		}
	}
	// Follow up with a valid query to prove server is still healthy.
	q := buildDNSQuery(0x9999, 0x0100, "healthy.test", 1, 1, nil)
	resp := udpExchange(t, srv.DNSAddr(), q)
	if gotID := binary.BigEndian.Uint16(resp[0:2]); gotID != 0x9999 {
		t.Fatalf("post-malformed query id = %#x, want 0x9999", gotID)
	}
	if an := binary.BigEndian.Uint16(resp[6:8]); an != 1 {
		t.Fatalf("post-malformed query ANCOUNT = %d, want 1", an)
	}
}

func buildDHCPPacket(xid uint32, mac net.HardwareAddr, ciaddr net.IP, flags uint16, msgType byte, reqIP, serverID net.IP) []byte {
	buf := make([]byte, dhcpMinPacketLen)
	buf[0] = dhcpOpRequest
	buf[1] = 1 // Ethernet
	buf[2] = byte(len(mac))
	binary.BigEndian.PutUint32(buf[4:8], xid)
	binary.BigEndian.PutUint16(buf[10:12], flags)
	if ip4 := ciaddr.To4(); ip4 != nil {
		copy(buf[12:16], ip4)
	}
	copy(buf[28:28+len(mac)], mac)
	copy(buf[236:240], dhcpMagicCookie[:])

	buf = append(buf, optMessageType, 1, msgType)
	if ip4 := reqIP.To4(); ip4 != nil {
		buf = append(buf, optRequestedIP, 4)
		buf = append(buf, ip4...)
	}
	if ip4 := serverID.To4(); ip4 != nil {
		buf = append(buf, optServerID, 4)
		buf = append(buf, ip4...)
	}
	buf = append(buf, optEnd)
	return buf
}

func decodeAllDHCPOptions(t *testing.T, pkt []byte) map[byte][]byte {
	t.Helper()
	if len(pkt) < dhcpMinPacketLen {
		t.Fatalf("dhcp reply too short: %d", len(pkt))
	}
	if !bytes.Equal(pkt[236:240], dhcpMagicCookie[:]) {
		t.Fatalf("dhcp reply missing magic cookie: %x", pkt[236:240])
	}
	opts := make(map[byte][]byte)
	data := pkt[240:]
	for i := 0; i < len(data); {
		code := data[i]
		if code == optPad {
			i++
			continue
		}
		if code == optEnd {
			return opts
		}
		if i+1 >= len(data) {
			t.Fatalf("truncated dhcp option %d", code)
		}
		n := int(data[i+1])
		if i+2+n > len(data) {
			t.Fatalf("truncated dhcp option %d value (len %d)", code, n)
		}
		opts[code] = append([]byte(nil), data[i+2:i+2+n]...)
		i += 2 + n
	}
	t.Fatalf("dhcp reply missing END option")
	return nil
}

func assertDHCPOptions(t *testing.T, opts map[byte][]byte, wantType byte, wantServer, wantMask net.IP, wantLease uint32, wantCaptive string) {
	t.Helper()
	if got := opts[optMessageType]; len(got) != 1 || got[0] != wantType {
		t.Fatalf("opt 53 (msg type) = %v, want %d", got, wantType)
	}
	if got := net.IP(opts[optServerID]); !got.Equal(wantServer) {
		t.Fatalf("opt 54 (server id) = %v, want %s", got, wantServer)
	}
	if got := opts[optLeaseTime]; len(got) != 4 || binary.BigEndian.Uint32(got) != wantLease {
		t.Fatalf("opt 51 (lease) = %v, want %d", got, wantLease)
	}
	if got := net.IP(opts[optSubnetMask]); !got.Equal(wantMask) {
		t.Fatalf("opt 1 (mask) = %v, want %s", got, wantMask)
	}
	if got := net.IP(opts[optRouter]); !got.Equal(wantServer) {
		t.Fatalf("opt 3 (router) = %v, want %s", got, wantServer)
	}
	if got := net.IP(opts[optDNS]); !got.Equal(wantServer) {
		t.Fatalf("opt 6 (dns) = %v, want %s", got, wantServer)
	}
	if got := string(opts[optCaptivePortal]); got != wantCaptive {
		t.Fatalf("opt 114 (captive portal) = %q, want %q", got, wantCaptive)
	}
}

func TestDHCPFlowOverUDP(t *testing.T) {
	srv := startLoopbackServer(t, Config{})

	mac1 := net.HardwareAddr{0xAA, 0xBB, 0xCC, 0x00, 0x00, 0x01}
	mac2 := net.HardwareAddr{0xAA, 0xBB, 0xCC, 0x00, 0x00, 0x02}
	mac3 := net.HardwareAddr{0xAA, 0xBB, 0xCC, 0x00, 0x00, 0x03}
	wantServer := net.IPv4(192, 168, 4, 1)
	wantMask := net.IPv4(255, 255, 255, 0)
	wantCaptive := "http://192.168.4.1/"

	// 1. DISCOVER -> OFFER for mac1 -> 192.168.4.10.
	disc1 := buildDHCPPacket(0x11223344, mac1, nil, dhcpFlagBroadcast, dhcpDiscover, nil, nil)
	offer1 := udpExchange(t, srv.DHCPAddr(), disc1)
	if offer1[0] != dhcpOpReply {
		t.Fatalf("offer op = %d, want %d", offer1[0], dhcpOpReply)
	}
	if xid := binary.BigEndian.Uint32(offer1[4:8]); xid != 0x11223344 {
		t.Fatalf("offer xid = %#x, want 0x11223344", xid)
	}
	ip1 := net.IP(offer1[16:20])
	if !ip1.Equal(net.IPv4(192, 168, 4, 10)) {
		t.Fatalf("offer1 yiaddr = %s, want 192.168.4.10", ip1)
	}
	assertDHCPOptions(t, decodeAllDHCPOptions(t, offer1), dhcpOffer, wantServer, wantMask, 3600, wantCaptive)

	// 2. REQUEST -> ACK for mac1.
	req1 := buildDHCPPacket(0x11223345, mac1, nil, dhcpFlagBroadcast, dhcpRequest, ip1, wantServer)
	ack1 := udpExchange(t, srv.DHCPAddr(), req1)
	if gotIP := net.IP(ack1[16:20]); !gotIP.Equal(ip1) {
		t.Fatalf("ack1 yiaddr = %s, want %s", gotIP, ip1)
	}
	assertDHCPOptions(t, decodeAllDHCPOptions(t, ack1), dhcpAck, wantServer, wantMask, 3600, wantCaptive)

	// 3. Same MAC DISCOVER again gets the same IP.
	offer1Again := udpExchange(t, srv.DHCPAddr(), buildDHCPPacket(0x11223346, mac1, nil, dhcpFlagBroadcast, dhcpDiscover, nil, nil))
	if gotIP := net.IP(offer1Again[16:20]); !gotIP.Equal(ip1) {
		t.Fatalf("same MAC re-discover yiaddr = %s, want %s", gotIP, ip1)
	}

	// 4. Second MAC gets next IP (192.168.4.11).
	offer2 := udpExchange(t, srv.DHCPAddr(), buildDHCPPacket(0x22334455, mac2, nil, dhcpFlagBroadcast, dhcpDiscover, nil, nil))
	ip2 := net.IP(offer2[16:20])
	if !ip2.Equal(net.IPv4(192, 168, 4, 11)) {
		t.Fatalf("offer2 yiaddr = %s, want 192.168.4.11", ip2)
	}
	ack2 := udpExchange(t, srv.DHCPAddr(), buildDHCPPacket(0x22334456, mac2, nil, dhcpFlagBroadcast, dhcpRequest, ip2, wantServer))
	if gotIP := net.IP(ack2[16:20]); !gotIP.Equal(ip2) {
		t.Fatalf("ack2 yiaddr = %s, want %s", gotIP, ip2)
	}

	// 5. Wrong-subnet REQUEST gets DHCPNAK.
	nakPkt := udpExchange(t, srv.DHCPAddr(), buildDHCPPacket(0x33445566, mac3, nil, dhcpFlagBroadcast, dhcpRequest, net.IPv4(192, 168, 1, 50), nil))
	nakOpts := decodeAllDHCPOptions(t, nakPkt)
	if got := nakOpts[optMessageType]; len(got) != 1 || got[0] != dhcpNak {
		t.Fatalf("wrong-subnet request msgType = %v, want NAK (%d)", got, dhcpNak)
	}
	if got := net.IP(nakOpts[optServerID]); !got.Equal(wantServer) {
		t.Fatalf("NAK server ID = %v, want %s", got, wantServer)
	}
	if yi := net.IP(nakPkt[16:20]); !yi.Equal(net.IPv4zero) {
		t.Fatalf("NAK yiaddr = %s, want 0.0.0.0", yi)
	}

	// 6. Requesting an IP held by another active MAC gets DHCPNAK.
	conflictNak := udpExchange(t, srv.DHCPAddr(), buildDHCPPacket(0x33445567, mac3, nil, dhcpFlagBroadcast, dhcpRequest, ip1, wantServer))
	if got := decodeAllDHCPOptions(t, conflictNak)[optMessageType]; len(got) != 1 || got[0] != dhcpNak {
		t.Fatalf("conflicting IP request msgType = %v, want NAK", got)
	}

	// 7. RELEASE frees mac1's IP (192.168.4.10) so mac3 gets it next.
	relConn, err := net.DialTimeout("udp4", srv.DHCPAddr().String(), time.Second)
	if err != nil {
		t.Fatalf("dial dhcp: %v", err)
	}
	defer relConn.Close()
	rel1 := buildDHCPPacket(0x44556677, mac1, ip1, 0, dhcpRelease, nil, wantServer)
	if _, err := relConn.Write(rel1); err != nil {
		t.Fatalf("write release: %v", err)
	}

	// Wait briefly until lease table reflects the release, then discover with mac3.
	deadline := time.Now().Add(time.Second)
	for time.Now().Before(deadline) {
		if _, exists := srv.DHCP.Leases()[mac1.String()]; !exists {
			break
		}
		time.Sleep(5 * time.Millisecond)
	}
	if _, exists := srv.DHCP.Leases()[mac1.String()]; exists {
		t.Fatalf("mac1 lease still active after RELEASE")
	}

	offer3 := udpExchange(t, srv.DHCPAddr(), buildDHCPPacket(0x55667788, mac3, nil, dhcpFlagBroadcast, dhcpDiscover, nil, nil))
	if gotIP := net.IP(offer3[16:20]); !gotIP.Equal(ip1) {
		t.Fatalf("offer3 after release yiaddr = %s, want reclaimed %s", gotIP, ip1)
	}
}

func TestDHCPExpiryAndReclamation(t *testing.T) {
	now := time.Date(2026, 11, 27, 12, 0, 0, 0, time.UTC)
	srv, err := NewDHCPServer(DHCPConfig{
		ServerIP:   net.IPv4(192, 168, 4, 1),
		Mask:       net.CIDRMask(24, 32),
		PoolStart:  net.IPv4(192, 168, 4, 10),
		PoolEnd:    net.IPv4(192, 168, 4, 11),
		Lease:      10 * time.Minute,
		CaptiveURL: "http://192.168.4.1/",
	})
	if err != nil {
		t.Fatalf("NewDHCPServer: %v", err)
	}
	srv.now = func() time.Time { return now }

	mac1 := net.HardwareAddr{0x02, 0x00, 0x00, 0x00, 0x00, 0x01}
	mac2 := net.HardwareAddr{0x02, 0x00, 0x00, 0x00, 0x00, 0x02}
	mac3 := net.HardwareAddr{0x02, 0x00, 0x00, 0x00, 0x00, 0x03}

	// Fill 2-IP pool with mac1 (.10) and mac2 (.11).
	r1, ok := srv.HandlePacket(buildDHCPPacket(1, mac1, nil, 0, dhcpRequest, net.IPv4(192, 168, 4, 10), net.IPv4(192, 168, 4, 1)))
	if !ok || r1.msgType != dhcpAck {
		t.Fatalf("mac1 request failed: ok=%v", ok)
	}
	now = now.Add(time.Minute)
	r2, ok := srv.HandlePacket(buildDHCPPacket(2, mac2, nil, 0, dhcpRequest, net.IPv4(192, 168, 4, 11), net.IPv4(192, 168, 4, 1)))
	if !ok || r2.msgType != dhcpAck {
		t.Fatalf("mac2 request failed: ok=%v", ok)
	}

	// Pool full -> mac3 DISCOVER gets no reply.
	if _, ok := srv.HandlePacket(buildDHCPPacket(3, mac3, nil, 0, dhcpDiscover, nil, nil)); ok {
		t.Fatalf("expected pool exhaustion for mac3")
	}

	// Advance past mac1's expiry (10m from t=0) but before mac2's expiry (11m).
	now = now.Add(9*time.Minute + time.Second)
	r3, ok := srv.HandlePacket(buildDHCPPacket(4, mac3, nil, 0, dhcpDiscover, nil, nil))
	if !ok || r3.msgType != dhcpOffer {
		t.Fatalf("expected reclaimed offer for mac3")
	}
	if gotIP := net.IP(r3.pkt[16:20]); !gotIP.Equal(net.IPv4(192, 168, 4, 10)) {
		t.Fatalf("reclaimed IP = %s, want 192.168.4.10", gotIP)
	}

	// Wrong Server ID in Option 54 is silently ignored.
	if _, ok := srv.HandlePacket(buildDHCPPacket(5, mac3, nil, 0, dhcpRequest, net.IPv4(192, 168, 4, 10), net.IPv4(192, 168, 4, 99))); ok {
		t.Fatalf("expected request for another server ID to be ignored")
	}

	// DHCPDECLINE releases lease cleanly.
	if _, ok := srv.HandlePacket(buildDHCPPacket(6, mac2, nil, 0, dhcpDecline, net.IPv4(192, 168, 4, 11), net.IPv4(192, 168, 4, 1))); ok {
		t.Fatalf("expected no reply to DHCPDECLINE")
	}
	if _, exists := srv.Leases()[mac2.String()]; exists {
		t.Fatalf("mac2 lease still present after DECLINE")
	}

	// DHCPINFORM replies with ACK, yiaddr=0.0.0.0, options including 114, no option 51.
	inf, ok := srv.HandlePacket(buildDHCPPacket(7, mac3, net.IPv4(192, 168, 4, 10), 0, dhcpInform, nil, nil))
	if !ok || inf.msgType != dhcpAck {
		t.Fatalf("expected ACK for DHCPINFORM")
	}
	if yi := net.IP(inf.pkt[16:20]); !yi.Equal(net.IPv4zero) {
		t.Fatalf("INFORM ACK yiaddr = %s, want 0.0.0.0", yi)
	}
	infOpts := decodeAllDHCPOptions(t, inf.pkt)
	if _, hasLease := infOpts[optLeaseTime]; hasLease {
		t.Fatalf("INFORM ACK must not include option 51 (lease time)")
	}
	if got := string(infOpts[optCaptivePortal]); got != "http://192.168.4.1/" {
		t.Fatalf("INFORM ACK opt 114 = %q, want http://192.168.4.1/", got)
	}
}

func TestDefaultDHCPReplyAddr(t *testing.T) {
	ci := net.IPv4(192, 168, 4, 10)
	src := &net.UDPAddr{IP: net.IPv4(192, 168, 4, 10), Port: 68}

	cases := []struct {
		name      string
		ciaddr    net.IP
		broadcast bool
		msgType   byte
		want      string
	}{
		{"offer_no_ciaddr", net.IPv4zero, false, dhcpOffer, "255.255.255.255:68"},
		{"ack_unicast_renew", ci, false, dhcpAck, "192.168.4.10:68"},
		{"ack_broadcast_flag", ci, true, dhcpAck, "255.255.255.255:68"},
		{"nak_always_broadcast", ci, false, dhcpNak, "255.255.255.255:68"},
	}
	for _, tc := range cases {
		got := defaultDHCPReplyAddr(src, tc.ciaddr, tc.broadcast, tc.msgType)
		if got.String() != tc.want {
			t.Errorf("%s: got %s, want %s", tc.name, got, tc.want)
		}
	}
}

func TestServerCleanShutdownAndCancel(t *testing.T) {
	baseGoroutines := runtime.NumGoroutine()

	// Shutdown via Close() with Background context.
	srv, err := Start(context.Background(), Config{
		DHCPAddr: "127.0.0.1:0",
		DNSAddr:  "127.0.0.1:0",
	})
	if err != nil {
		t.Fatalf("Start: %v", err)
	}
	if err := srv.Close(); err != nil {
		t.Fatalf("Close 1: %v", err)
	}
	if err := srv.Close(); err != nil {
		t.Fatalf("Close 2: %v", err)
	}

	// Shutdown via context cancellation.
	ctx, cancel := context.WithCancel(context.Background())
	srv2, err := Start(ctx, Config{
		DHCPAddr: "127.0.0.1:0",
		DNSAddr:  "127.0.0.1:0",
	})
	if err != nil {
		t.Fatalf("Start 2: %v", err)
	}
	cancel()
	if err := srv2.Close(); err != nil {
		t.Fatalf("Close after cancel: %v", err)
	}

	// Allow runtime scheduler a brief moment to reap exited goroutines.
	deadline := time.Now().Add(time.Second)
	for runtime.NumGoroutine() > baseGoroutines && time.Now().Before(deadline) {
		time.Sleep(5 * time.Millisecond)
	}
	if got := runtime.NumGoroutine(); got > baseGoroutines {
		t.Fatalf("goroutine leak: started with %d, ended with %d", baseGoroutines, got)
	}
}
