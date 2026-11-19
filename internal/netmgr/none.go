package netmgr

import (
	"context"
	"errors"
	"sort"
	"strings"

	"github.com/emdioh/wa-transcriber/internal/status"
)

// errDisabled is returned by mutating operations in mode "none".
var errDisabled = errors.New("network management is disabled (mode none)")

// none reports addresses only; it never changes the network. The host owns
// networking in Docker and on dev machines.
type none struct {
	addrs func() []status.Addr
}

func (n *none) Start(context.Context) error { return nil }

func (n *none) Status() Status {
	if n.addrs == nil {
		return Status{}
	}
	return StatusFromAddrs(n.addrs())
}

func (n *none) Scan(context.Context) ([]Network, error) { return nil, errDisabled }
func (n *none) SaveNetwork(string, string) error        { return errDisabled }
func (n *none) ForgetNetwork(string) error              { return errDisabled }
func (n *none) SetAPPolicy(APPolicy) error              { return errDisabled }

// InAPMode is always false in mode none.
func (n *none) InAPMode() bool { return false }

// APPolicy reports the default policy (none is never used in mode none).
func (n *none) APPolicy() APPolicy { return APAuto }

// SavedNetworks is empty in mode none.
func (n *none) SavedNetworks() []SavedNetwork { return nil }

// Erase is a no-op in mode none: there are no saved networks.
func (n *none) Erase() error { return nil }

// StatusFromAddrs groups non-loopback IPv4 addresses by interface and guesses
// the kind from the interface name. It is what mode "none" reports.
func StatusFromAddrs(addrs []status.Addr) Status {
	byName := map[string]*Interface{}
	for _, a := range addrs {
		ifc, ok := byName[a.Interface]
		if !ok {
			ifc = &Interface{Name: a.Interface, Kind: guessKind(a.Interface), State: StateUp}
			byName[a.Interface] = ifc
		}
		ifc.IPs = append(ifc.IPs, a.IP)
	}
	names := make([]string, 0, len(byName))
	for name := range byName {
		names = append(names, name)
	}
	sort.Strings(names)
	out := Status{Interfaces: make([]Interface, 0, len(names))}
	for _, name := range names {
		ifc := byName[name]
		sort.Strings(ifc.IPs)
		out.Interfaces = append(out.Interfaces, *ifc)
	}
	return out
}

func guessKind(name string) Kind {
	switch lower := strings.ToLower(name); {
	case strings.HasPrefix(lower, "eth"), strings.HasPrefix(lower, "en"):
		return KindEthernet
	case strings.HasPrefix(lower, "wl"):
		return KindWiFi
	default:
		return KindOther
	}
}
