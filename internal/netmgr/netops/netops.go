// Package netops implements pure-Go Linux network operations (RTNETLINK link,
// address, and route management, AF_PACKET RFC 5227 ARP probing, and nl80211
// station counting) without shelling out to ip, arping, or iw.
package netops

import (
	"context"
	"encoding/binary"
	"errors"
	"fmt"
	"net"
)

// ErrUnsupported is returned by Default() operations on non-Linux platforms.
var ErrUnsupported = errors.New("netops: unsupported platform")

// Ops abstracts the host network operations used by netmgr so callers can
// inject fakes in unit tests.
type Ops interface {
	LinkUp(ifname string) error
	LinkDown(ifname string) error
	AddrAdd(ifname string, ip net.IP, prefixLen int) error
	AddrReplace(ifname string, ip net.IP, prefixLen int) error
	AddrDel(ifname string, ip net.IP, prefixLen int) error
	AddrFlush(ifname string) error
	// AddrReplacePrimary ensures ip/prefixLen is configured on ifname
	// (idempotent via NLM_F_REPLACE|NLM_F_CREATE) and removes any other
	// IPv4 addresses on ifname that differ from ip/prefixLen (D16!).
	AddrReplacePrimary(ifname string, ip net.IP, prefixLen int) error
	RouteReplaceDefault(ifname string, gw net.IP) error
	// ARPProbe sends RFC 5227 ARP probes for ip on ifname. Returns true if
	// a conflict (another host owns ip) was detected.
	ARPProbe(ctx context.Context, ifname string, mac net.HardwareAddr, ip net.IP) (inUse bool, err error)
	// StationCount returns the number of WiFi clients associated with AP ifname.
	StationCount(ifname string) (int, error)
}

// Default returns the native Ops implementation for the current platform
// (pure-Go netlink / AF_PACKET on Linux, an unsupported stub elsewhere).
func Default() Ops {
	return newDefault()
}

// align4 rounds n up to a 4-byte boundary (NLMSG_ALIGN / RTA_ALIGN / NLA_ALIGN).
func align4(n int) int {
	return (n + 3) &^ 3
}

// htons converts a 16-bit host-order value to network byte order (big-endian)
// stored in a host uint16, as expected by Linux AF_PACKET socket/bind calls.
func htons(v uint16) uint16 {
	var b [2]byte
	binary.BigEndian.PutUint16(b[:], v)
	return binary.NativeEndian.Uint16(b[:])
}

func validateIPv4(ip net.IP) (net.IP, error) {
	if ip == nil {
		return nil, fmt.Errorf("netops: nil IPv4 address")
	}
	ip4 := ip.To4()
	if ip4 == nil {
		return nil, fmt.Errorf("netops: not an IPv4 address: %q", ip.String())
	}
	return ip4, nil
}

func validateIPv4Prefix(ip net.IP, prefixLen int) (net.IP, error) {
	ip4, err := validateIPv4(ip)
	if err != nil {
		return nil, err
	}
	if prefixLen < 0 || prefixLen > 32 {
		return nil, fmt.Errorf("netops: invalid IPv4 prefix length %d", prefixLen)
	}
	return ip4, nil
}

// ipv4Broadcast computes the subnet broadcast address for ip4/prefixLen.
// For /31 (RFC 3021 point-to-point) and /32 (host) it returns nil so callers
// omit IFA_BROADCAST.
func ipv4Broadcast(ip4 net.IP, prefixLen int) net.IP {
	if prefixLen > 30 || prefixLen < 0 {
		return nil
	}
	mask := net.CIDRMask(prefixLen, 32)
	brd := make(net.IP, 4)
	for i := 0; i < 4; i++ {
		brd[i] = ip4[i] | ^mask[i]
	}
	return brd
}
