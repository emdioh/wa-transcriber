//go:build linux

package netops

import (
	"context"
	"errors"
	"fmt"
	"net"
	"time"

	"golang.org/x/sys/unix"
)

const (
	arpProbeCount   = 2
	arpProbeWait    = 400 * time.Millisecond
	arpPollMaxSlice = 50 * time.Millisecond
)

func (o *linuxOps) ARPProbe(ctx context.Context, ifname string, mac net.HardwareAddr, ip net.IP) (bool, error) {
	if err := ctx.Err(); err != nil {
		return false, err
	}
	ip4, err := validateIPv4(ip)
	if err != nil {
		return false, err
	}
	iface, err := net.InterfaceByName(ifname)
	if err != nil {
		return false, fmt.Errorf("netops: arp probe %s on %q: %w", ip4, ifname, err)
	}
	if len(mac) == 0 {
		mac = iface.HardwareAddr
	}
	frame, err := encodeARPProbe(mac, ip4)
	if err != nil {
		return false, fmt.Errorf("netops: arp probe %s on %q: %w", ip4, ifname, err)
	}

	proto := htons(ethProtoARP)
	fd, err := unix.Socket(unix.AF_PACKET, unix.SOCK_RAW|unix.SOCK_CLOEXEC|unix.SOCK_NONBLOCK, int(proto))
	if err != nil {
		return false, fmt.Errorf("netops: open AF_PACKET socket on %q: %w", ifname, err)
	}
	defer unix.Close(fd)

	bindAddr := &unix.SockaddrLinklayer{
		Protocol: proto,
		Ifindex:  iface.Index,
	}
	if err := unix.Bind(fd, bindAddr); err != nil {
		return false, fmt.Errorf("netops: bind AF_PACKET socket to %q: %w", ifname, err)
	}

	dstAddr := &unix.SockaddrLinklayer{
		Protocol: proto,
		Ifindex:  iface.Index,
		Halen:    6,
		Addr:     [8]byte{0xff, 0xff, 0xff, 0xff, 0xff, 0xff},
	}

	buf := make([]byte, 1500)
	pfds := []unix.PollFd{{Fd: int32(fd), Events: unix.POLLIN}}

	for attempt := 0; attempt < arpProbeCount; attempt++ {
		if err := ctx.Err(); err != nil {
			return false, err
		}
		if err := sendPacket(fd, frame, dstAddr); err != nil {
			return false, fmt.Errorf("netops: send ARP probe on %q: %w", ifname, err)
		}

		deadline := time.Now().Add(arpProbeWait)
		for {
			if err := ctx.Err(); err != nil {
				return false, err
			}
			rem := time.Until(deadline)
			if rem <= 0 {
				break
			}
			wait := rem
			if wait > arpPollMaxSlice {
				wait = arpPollMaxSlice
			}
			ms := int(wait.Milliseconds())
			if ms < 1 {
				ms = 1
			}

			pfds[0].Revents = 0
			n, err := unix.Poll(pfds, ms)
			if errors.Is(err, unix.EINTR) {
				continue
			}
			if err != nil {
				return false, fmt.Errorf("netops: poll ARP socket on %q: %w", ifname, err)
			}
			if n == 0 {
				continue
			}
			if pfds[0].Revents&(unix.POLLERR|unix.POLLHUP|unix.POLLNVAL) != 0 {
				return false, fmt.Errorf("netops: ARP socket error on %q (revents=0x%x)", ifname, pfds[0].Revents)
			}
			if pfds[0].Revents&unix.POLLIN == 0 {
				continue
			}

			// Drain all queued frames.
			for {
				rn, _, err := unix.Recvfrom(fd, buf, 0)
				if errors.Is(err, unix.EINTR) {
					continue
				}
				if errors.Is(err, unix.EAGAIN) || errors.Is(err, unix.EWOULDBLOCK) {
					break
				}
				if err != nil {
					return false, fmt.Errorf("netops: recv ARP frame on %q: %w", ifname, err)
				}
				if isARPConflict(buf[:rn], mac, ip4) {
					return true, nil
				}
			}
		}
	}

	return false, nil
}

func sendPacket(fd int, frame []byte, dst *unix.SockaddrLinklayer) error {
	for {
		err := unix.Sendto(fd, frame, 0, dst)
		if errors.Is(err, unix.EINTR) {
			continue
		}
		return err
	}
}
