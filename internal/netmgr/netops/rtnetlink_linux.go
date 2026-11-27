//go:build linux

package netops

import (
	"errors"
	"fmt"
	"net"
	"sync/atomic"
	"time"

	"golang.org/x/sys/unix"
)

const netlinkTimeout = 5 * time.Second

type linuxOps struct {
	seq atomic.Uint32
}

func newDefault() Ops {
	return &linuxOps{}
}

func (o *linuxOps) nextSeq() uint32 {
	for {
		if s := o.seq.Add(1); s != 0 {
			return s
		}
	}
}

func openNetlinkSocket(proto int) (int, error) {
	fd, err := unix.Socket(unix.AF_NETLINK, unix.SOCK_RAW|unix.SOCK_CLOEXEC, proto)
	if err != nil {
		return -1, fmt.Errorf("netops: open netlink socket (proto=%d): %w", proto, err)
	}
	if err := unix.Bind(fd, &unix.SockaddrNetlink{Family: unix.AF_NETLINK}); err != nil {
		_ = unix.Close(fd)
		return -1, fmt.Errorf("netops: bind netlink socket: %w", err)
	}
	tv := unix.NsecToTimeval(netlinkTimeout.Nanoseconds())
	if err := unix.SetsockoptTimeval(fd, unix.SOL_SOCKET, unix.SO_RCVTIMEO, &tv); err != nil {
		_ = unix.Close(fd)
		return -1, fmt.Errorf("netops: set netlink SO_RCVTIMEO: %w", err)
	}
	return fd, nil
}

func sendNetlink(fd int, req []byte) error {
	dst := &unix.SockaddrNetlink{Family: unix.AF_NETLINK}
	for {
		err := unix.Sendto(fd, req, 0, dst)
		if errors.Is(err, unix.EINTR) {
			continue
		}
		return err
	}
}

func recvNetlink(fd int, buf []byte) (int, error) {
	for {
		n, from, err := unix.Recvfrom(fd, buf, 0)
		if errors.Is(err, unix.EINTR) {
			continue
		}
		if err != nil {
			return 0, err
		}
		if nlFrom, ok := from.(*unix.SockaddrNetlink); ok && nlFrom.Pid != 0 {
			// Ignore messages not originating from the kernel.
			continue
		}
		return n, nil
	}
}

// rtnetlinkAck sends a single RTNETLINK request expecting an NLMSG_ERROR ACK.
func (o *linuxOps) rtnetlinkAck(req []byte, seq uint32) error {
	fd, err := openNetlinkSocket(unix.NETLINK_ROUTE)
	if err != nil {
		return err
	}
	defer unix.Close(fd)

	if err := sendNetlink(fd, req); err != nil {
		return fmt.Errorf("netops: send rtnetlink request: %w", err)
	}

	buf := make([]byte, 8192)
	for {
		n, err := recvNetlink(fd, buf)
		if err != nil {
			return fmt.Errorf("netops: recv rtnetlink ack: %w", err)
		}
		msgs, err := parseNLMessages(buf[:n])
		if err != nil {
			return err
		}
		found, errno, err := parseNLAck(msgs, seq)
		if err != nil {
			return err
		}
		if found {
			if errno != 0 {
				return unix.Errno(errno)
			}
			return nil
		}
	}
}

func (o *linuxOps) LinkUp(ifname string) error {
	iface, err := net.InterfaceByName(ifname)
	if err != nil {
		return fmt.Errorf("netops: link up %q: %w", ifname, err)
	}
	seq := o.nextSeq()
	if err := o.rtnetlinkAck(encodeLinkState(iface.Index, true, seq), seq); err != nil {
		return fmt.Errorf("netops: link up %q: %w", ifname, err)
	}
	return nil
}

func (o *linuxOps) LinkDown(ifname string) error {
	iface, err := net.InterfaceByName(ifname)
	if err != nil {
		return fmt.Errorf("netops: link down %q: %w", ifname, err)
	}
	seq := o.nextSeq()
	if err := o.rtnetlinkAck(encodeLinkState(iface.Index, false, seq), seq); err != nil {
		return fmt.Errorf("netops: link down %q: %w", ifname, err)
	}
	return nil
}

func (o *linuxOps) AddrAdd(ifname string, ip net.IP, prefixLen int) error {
	ip4, err := validateIPv4Prefix(ip, prefixLen)
	if err != nil {
		return err
	}
	iface, err := net.InterfaceByName(ifname)
	if err != nil {
		return fmt.Errorf("netops: addr add %s/%d on %q: %w", ip4, prefixLen, ifname, err)
	}
	seq := o.nextSeq()
	if err := o.rtnetlinkAck(encodeAddrAdd(iface.Index, ip4, prefixLen, seq), seq); err != nil {
		return fmt.Errorf("netops: addr add %s/%d on %q: %w", ip4, prefixLen, ifname, err)
	}
	return nil
}

func (o *linuxOps) AddrReplace(ifname string, ip net.IP, prefixLen int) error {
	ip4, err := validateIPv4Prefix(ip, prefixLen)
	if err != nil {
		return err
	}
	iface, err := net.InterfaceByName(ifname)
	if err != nil {
		return fmt.Errorf("netops: addr replace %s/%d on %q: %w", ip4, prefixLen, ifname, err)
	}
	seq := o.nextSeq()
	if err := o.rtnetlinkAck(encodeAddrReplace(iface.Index, ip4, prefixLen, seq), seq); err != nil {
		if errors.Is(err, unix.EEXIST) {
			return nil
		}
		return fmt.Errorf("netops: addr replace %s/%d on %q: %w", ip4, prefixLen, ifname, err)
	}
	return nil
}

func (o *linuxOps) AddrDel(ifname string, ip net.IP, prefixLen int) error {
	ip4, err := validateIPv4Prefix(ip, prefixLen)
	if err != nil {
		return err
	}
	iface, err := net.InterfaceByName(ifname)
	if err != nil {
		return fmt.Errorf("netops: addr del %s/%d on %q: %w", ip4, prefixLen, ifname, err)
	}
	seq := o.nextSeq()
	if err := o.rtnetlinkAck(encodeAddrDel(iface.Index, ip4, prefixLen, seq), seq); err != nil {
		return fmt.Errorf("netops: addr del %s/%d on %q: %w", ip4, prefixLen, ifname, err)
	}
	return nil
}

type ipv4CIDR struct {
	ip        net.IP
	prefixLen int
}

func listIPv4Addrs(ifname string) ([]ipv4CIDR, error) {
	iface, err := net.InterfaceByName(ifname)
	if err != nil {
		return nil, err
	}
	addrs, err := iface.Addrs()
	if err != nil {
		return nil, err
	}
	var out []ipv4CIDR
	for _, a := range addrs {
		ipNet, ok := a.(*net.IPNet)
		if !ok {
			continue
		}
		ip4 := ipNet.IP.To4()
		if ip4 == nil {
			continue
		}
		ones, bits := ipNet.Mask.Size()
		if bits != 32 {
			continue
		}
		out = append(out, ipv4CIDR{ip: ip4, prefixLen: ones})
	}
	return out, nil
}

func (o *linuxOps) AddrFlush(ifname string) error {
	addrs, err := listIPv4Addrs(ifname)
	if err != nil {
		return fmt.Errorf("netops: addr flush %q: %w", ifname, err)
	}
	for _, a := range addrs {
		if err := o.AddrDel(ifname, a.ip, a.prefixLen); err != nil {
			if errors.Is(err, unix.EADDRNOTAVAIL) {
				continue
			}
			return err
		}
	}
	return nil
}

func (o *linuxOps) AddrReplacePrimary(ifname string, ip net.IP, prefixLen int) error {
	ip4, err := validateIPv4Prefix(ip, prefixLen)
	if err != nil {
		return err
	}
	if err := o.AddrReplace(ifname, ip4, prefixLen); err != nil {
		return err
	}
	addrs, err := listIPv4Addrs(ifname)
	if err != nil {
		return fmt.Errorf("netops: addr replace primary %q: list addrs: %w", ifname, err)
	}
	for _, a := range addrs {
		if a.ip.Equal(ip4) && a.prefixLen == prefixLen {
			continue
		}
		if err := o.AddrDel(ifname, a.ip, a.prefixLen); err != nil {
			if errors.Is(err, unix.EADDRNOTAVAIL) {
				continue
			}
			return err
		}
	}
	return nil
}

func (o *linuxOps) RouteReplaceDefault(ifname string, gw net.IP) error {
	gw4, err := validateIPv4(gw)
	if err != nil {
		return err
	}
	iface, err := net.InterfaceByName(ifname)
	if err != nil {
		return fmt.Errorf("netops: route replace default via %s on %q: %w", gw4, ifname, err)
	}
	seq := o.nextSeq()
	if err := o.rtnetlinkAck(encodeRouteReplaceDefault(iface.Index, gw4, seq), seq); err != nil {
		return fmt.Errorf("netops: route replace default via %s on %q: %w", gw4, ifname, err)
	}
	return nil
}
