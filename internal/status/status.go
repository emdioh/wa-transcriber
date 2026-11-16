// Package status tracks the runtime state shown by !status and the web UI.
package status

import (
	"net"
	"sync"
	"time"
)

// Tracker is safe for concurrent use.
type Tracker struct {
	mu                sync.Mutex
	started           time.Time
	connected         bool
	account           string
	lastMessage       time.Time
	lastTranscription time.Time
	pairingCode       string // QR payload or pairing code while unpaired
}

// Snapshot is a consistent copy of the tracker.
type Snapshot struct {
	Started           time.Time
	Connected         bool
	Account           string // own phone number, "" when not paired
	LastMessage       time.Time
	LastTranscription time.Time
	PairingCode       string
}

func New(now time.Time) *Tracker { return &Tracker{started: now} }

func (t *Tracker) SetConnected(v bool) {
	t.mu.Lock()
	t.connected = v
	t.mu.Unlock()
}

func (t *Tracker) SetAccount(a string) {
	t.mu.Lock()
	t.account = a
	t.mu.Unlock()
}

func (t *Tracker) MessageSeen(at time.Time) {
	t.mu.Lock()
	if at.After(t.lastMessage) {
		t.lastMessage = at
	}
	t.mu.Unlock()
}

func (t *Tracker) Transcribed(at time.Time) {
	t.mu.Lock()
	t.lastTranscription = at
	t.mu.Unlock()
}

func (t *Tracker) SetPairingCode(c string) {
	t.mu.Lock()
	t.pairingCode = c
	t.mu.Unlock()
}

func (t *Tracker) Snapshot() Snapshot {
	t.mu.Lock()
	defer t.mu.Unlock()
	return Snapshot{
		Started:           t.started,
		Connected:         t.connected,
		Account:           t.account,
		LastMessage:       t.lastMessage,
		LastTranscription: t.lastTranscription,
		PairingCode:       t.pairingCode,
	}
}

// Addr is one IPv4 address of a network interface.
type Addr struct {
	Interface string
	IP        string
}

// Addresses lists the non-loopback IPv4 addresses of interfaces that are up.
func Addresses() []Addr {
	ifaces, err := net.Interfaces()
	if err != nil {
		return nil
	}
	var out []Addr
	for _, ifc := range ifaces {
		if ifc.Flags&net.FlagUp == 0 || ifc.Flags&net.FlagLoopback != 0 {
			continue
		}
		addrs, err := ifc.Addrs()
		if err != nil {
			continue
		}
		for _, a := range addrs {
			ipn, ok := a.(*net.IPNet)
			if !ok || ipn.IP.To4() == nil {
				continue
			}
			out = append(out, Addr{Interface: ifc.Name, IP: ipn.IP.String()})
		}
	}
	return out
}
