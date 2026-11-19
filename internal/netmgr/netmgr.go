// Package netmgr manages the device's network interfaces.
//
// Mode "none" (the default) only reports addresses and is for Docker and
// dev machines, where the host owns the network. Mode "managed" drives
// ethernet, WiFi and the setup access point on the device itself; it is
// added in the following milestones steps.
package netmgr

import (
	"context"
	"fmt"

	"github.com/emdioh/wa-transcriber/internal/status"
)

// Kind of a network interface.
type Kind string

const (
	KindEthernet Kind = "ethernet"
	KindWiFi     Kind = "wifi"
	KindOther    Kind = "other"
)

// State of a network interface.
type State string

const (
	StateDown State = "down"
	StateUp   State = "up"
)

// Interface is one network interface's runtime status.
type Interface struct {
	Name      string
	Kind      Kind
	State     State
	IPs       []string
	SSID      string
	APClients int
}

// Status is the network manager's view of the device.
type Status struct {
	Interfaces []Interface
}

// APPolicy controls when the setup access point is used (managed mode only).
type APPolicy string

const (
	APAuto   APPolicy = "auto"
	APAlways APPolicy = "always"
	APNever  APPolicy = "never"
)

// Network is a WiFi network found by a scan (managed mode only).
type Network struct {
	SSID    string
	Signal  int
	Secured bool
}

// Manager controls the network. Mode "none" never changes anything and only
// reports addresses; mode "managed" owns ethernet, WiFi and the setup AP.
type Manager interface {
	Start(ctx context.Context) error
	Status() Status
	Scan(ctx context.Context) ([]Network, error)
	SaveNetwork(ssid, passphrase string) error
	ForgetNetwork(ssid string) error
	SetAPPolicy(p APPolicy) error
	APPolicy() APPolicy
	// SavedNetworks lists the remembered WiFi networks (mode managed only).
	SavedNetworks() []SavedNetwork
	// InAPMode reports whether the setup access point is running.
	InAPMode() bool
	Erase() error
}

// New returns the manager for mode ("none" or "managed").
func New(mode string) (Manager, error) {
	return NewWithData(mode, "/data")
}

// NewWithData is New with an explicit data directory (managed mode keeps its
// saved WiFi networks there).
func NewWithData(mode, dataDir string) (Manager, error) {
	switch mode {
	case "none":
		return &none{addrs: status.Addresses}, nil
	case "managed":
		return NewManaged(dataDir, nil)
	default:
		return nil, fmt.Errorf("unknown -network mode %q (want none or managed)", mode)
	}
}
