//go:build linux

package dhcpc

import (
	"context"
	"fmt"
	"net"
	"syscall"

	"golang.org/x/sys/unix"
)

var broadcastAddr = &net.UDPAddr{
	IP:   net.IPv4bcast,
	Port: ServerPort,
}

func dialDefault(ctx context.Context, ifname string) (Transport, error) {
	if ifname == "" {
		return nil, fmt.Errorf("dhcpc: interface name is required")
	}
	lc := net.ListenConfig{
		Control: func(network, address string, c syscall.RawConn) error {
			var sockErr error
			if err := c.Control(func(fd uintptr) {
				if err := unix.SetsockoptInt(int(fd), unix.SOL_SOCKET, unix.SO_REUSEADDR, 1); err != nil {
					sockErr = fmt.Errorf("SO_REUSEADDR: %w", err)
					return
				}
				if err := unix.SetsockoptInt(int(fd), unix.SOL_SOCKET, unix.SO_BROADCAST, 1); err != nil {
					sockErr = fmt.Errorf("SO_BROADCAST: %w", err)
					return
				}
				if err := unix.BindToDevice(int(fd), ifname); err != nil {
					sockErr = fmt.Errorf("SO_BINDTODEVICE %q: %w", ifname, err)
					return
				}
			}); err != nil {
				return err
			}
			return sockErr
		},
	}
	pc, err := lc.ListenPacket(ctx, "udp4", fmt.Sprintf("0.0.0.0:%d", ClientPort))
	if err != nil {
		return nil, fmt.Errorf("dhcpc: listen udp4 :%d on %s: %w", ClientPort, ifname, err)
	}
	return NewUDPTransport(pc.(*net.UDPConn), broadcastAddr), nil
}
