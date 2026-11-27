//go:build linux

package apsrv

import (
	"context"
	"fmt"
	"net"
	"syscall"

	"golang.org/x/sys/unix"
)

func listenUDP(ctx context.Context, addr, ifName string, broadcast bool) (net.PacketConn, error) {
	lc := net.ListenConfig{
		Control: func(_, _ string, c syscall.RawConn) error {
			var sockErr error
			err := c.Control(func(fd uintptr) {
				if err := unix.SetsockoptInt(int(fd), unix.SOL_SOCKET, unix.SO_REUSEADDR, 1); err != nil {
					sockErr = fmt.Errorf("SO_REUSEADDR: %w", err)
					return
				}
				if broadcast {
					if err := unix.SetsockoptInt(int(fd), unix.SOL_SOCKET, unix.SO_BROADCAST, 1); err != nil {
						sockErr = fmt.Errorf("SO_BROADCAST: %w", err)
						return
					}
				}
				if ifName != "" {
					if err := unix.SetsockoptString(int(fd), unix.SOL_SOCKET, unix.SO_BINDTODEVICE, ifName); err != nil {
						sockErr = fmt.Errorf("SO_BINDTODEVICE %q: %w", ifName, err)
						return
					}
				}
			})
			if err != nil {
				return err
			}
			return sockErr
		},
	}
	return lc.ListenPacket(ctx, "udp4", addr)
}
