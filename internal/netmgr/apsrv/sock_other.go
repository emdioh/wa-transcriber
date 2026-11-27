//go:build !linux

package apsrv

import (
	"context"
	"net"
)

func listenUDP(ctx context.Context, addr, _ string, _ bool) (net.PacketConn, error) {
	var lc net.ListenConfig
	return lc.ListenPacket(ctx, "udp4", addr)
}
