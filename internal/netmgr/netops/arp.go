package netops

import (
	"bytes"
	"encoding/binary"
	"fmt"
	"net"
)

// Ethernet and ARP wire constants (RFC 826 / RFC 5227).
const (
	ethHdrLen   = 14
	arpPktLen   = 28
	arpFrameLen = ethHdrLen + arpPktLen // 42 bytes

	ethProtoARP  uint16 = 0x0806
	ethProtoIPv4 uint16 = 0x0800

	arpHTypeEthernet uint16 = 1
	arpHLenEthernet  byte   = 6
	arpPLenIPv4      byte   = 4

	arpOpRequest uint16 = 1
	arpOpReply   uint16 = 2
)

var (
	ethBroadcast = [6]byte{0xff, 0xff, 0xff, 0xff, 0xff, 0xff}
	ipv4Zero     = [4]byte{0, 0, 0, 0}
)

// encodeARPProbe builds a 42-byte Ethernet + RFC 5227 ARP Probe frame:
// dst=ff:ff:ff:ff:ff:ff, src=mac, ethertype=0x0806, op=REQUEST,
// SHA=mac, SPA=0.0.0.0, THA=00:00:00:00:00:00, TPA=targetIP.
func encodeARPProbe(mac net.HardwareAddr, targetIP net.IP) ([]byte, error) {
	if len(mac) != 6 {
		return nil, fmt.Errorf("netops: invalid Ethernet MAC length %d", len(mac))
	}
	ip4, err := validateIPv4(targetIP)
	if err != nil {
		return nil, err
	}

	frame := make([]byte, arpFrameLen)
	// Ethernet header
	copy(frame[0:6], ethBroadcast[:])
	copy(frame[6:12], mac)
	binary.BigEndian.PutUint16(frame[12:14], ethProtoARP)

	// ARP payload
	arp := frame[ethHdrLen:]
	binary.BigEndian.PutUint16(arp[0:2], arpHTypeEthernet)
	binary.BigEndian.PutUint16(arp[2:4], ethProtoIPv4)
	arp[4] = arpHLenEthernet
	arp[5] = arpPLenIPv4
	binary.BigEndian.PutUint16(arp[6:8], arpOpRequest)
	copy(arp[8:14], mac) // SHA
	// arp[14:18] = SPA (0.0.0.0)
	// arp[18:24] = THA (00:00:00:00:00:00)
	copy(arp[24:28], ip4) // TPA

	return frame, nil
}

// isARPConflict reports whether an incoming Ethernet frame indicates another
// host owns or is simultaneously probing for targetIP (RFC 5227 Section 2.1.1).
//
// A frame is a conflict if it is a valid Ethernet/IPv4 ARP packet from a
// different sender MAC (SHA != selfMAC) and either:
//   - Sender Protocol Address (SPA) equals targetIP (ARP reply, announcement, or
//     request from a host using targetIP), or
//   - It is an RFC 5227 ARP probe (op == REQUEST, SPA == 0.0.0.0, TPA == targetIP).
func isARPConflict(frame []byte, selfMAC net.HardwareAddr, targetIP net.IP) bool {
	if len(frame) < arpFrameLen || len(selfMAC) != 6 {
		return false
	}
	ip4 := targetIP.To4()
	if ip4 == nil {
		return false
	}

	// Verify Ethernet type is ARP.
	if binary.BigEndian.Uint16(frame[12:14]) != ethProtoARP {
		return false
	}

	arp := frame[ethHdrLen:]
	if binary.BigEndian.Uint16(arp[0:2]) != arpHTypeEthernet ||
		binary.BigEndian.Uint16(arp[2:4]) != ethProtoIPv4 ||
		arp[4] != arpHLenEthernet ||
		arp[5] != arpPLenIPv4 {
		return false
	}

	op := binary.BigEndian.Uint16(arp[6:8])
	if op != arpOpRequest && op != arpOpReply {
		return false
	}

	sha := arp[8:14]
	if bytes.Equal(sha, selfMAC) {
		return false
	}

	spa := arp[14:18]
	tpa := arp[24:28]

	if bytes.Equal(spa, ip4) {
		return true
	}
	if op == arpOpRequest && bytes.Equal(spa, ipv4Zero[:]) && bytes.Equal(tpa, ip4) {
		return true
	}
	return false
}
