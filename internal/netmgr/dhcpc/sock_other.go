//go:build !linux

package dhcpc

import (
	"context"
	"errors"
)

var errUnsupported = errors.New("dhcpc: default DHCP socket transport is only supported on Linux")

func dialDefault(ctx context.Context, ifname string) (Transport, error) {
	return nil, errUnsupported
}
