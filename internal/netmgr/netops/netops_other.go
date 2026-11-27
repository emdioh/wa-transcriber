//go:build !linux

package netops

import (
	"context"
	"net"
)

type unsupportedOps struct{}

func newDefault() Ops {
	return unsupportedOps{}
}

func (unsupportedOps) LinkUp(string) error                          { return ErrUnsupported }
func (unsupportedOps) LinkDown(string) error                        { return ErrUnsupported }
func (unsupportedOps) AddrAdd(string, net.IP, int) error            { return ErrUnsupported }
func (unsupportedOps) AddrReplace(string, net.IP, int) error        { return ErrUnsupported }
func (unsupportedOps) AddrDel(string, net.IP, int) error            { return ErrUnsupported }
func (unsupportedOps) AddrFlush(string) error                       { return ErrUnsupported }
func (unsupportedOps) AddrReplacePrimary(string, net.IP, int) error { return ErrUnsupported }
func (unsupportedOps) RouteReplaceDefault(string, net.IP) error     { return ErrUnsupported }
func (unsupportedOps) ARPProbe(context.Context, string, net.HardwareAddr, net.IP) (bool, error) {
	return false, ErrUnsupported
}
func (unsupportedOps) StationCount(string) (int, error) { return 0, ErrUnsupported }
