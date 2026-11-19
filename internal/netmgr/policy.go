package netmgr

import (
	"fmt"
	"time"
)

// wifiState is the state of one WiFi interface in the policy machine.
type wifiState string

const (
	stateOff        wifiState = "off"        // client mode disabled or nothing to try
	stateConnecting wifiState = "connecting" // trying saved networks
	stateConnected  wifiState = "connected"  // joined a saved network
	stateAP         wifiState = "ap"         // setup access point running
)

// action is what the loop must do to reach the new state.
type action string

const (
	actNone     action = ""
	actConnect  action = "connect"  // start/restart wpa_supplicant and try
	actStartAP  action = "start-ap" // bring the setup AP up
	actStopAP   action = "stop-ap"  // tear the setup AP down
	actSchedule action = "schedule" // stay off and re-evaluate later
)

// Timers from PLAN.md M5 step 8.
const (
	lostRetry      = 2 * time.Minute
	noStationsScan = 5 * time.Minute
	connectRetry   = 5 * time.Minute
)

// inputs are everything the policy looks at. The loop collects them.
type inputs struct {
	Saved         int           // saved networks
	SavedVisible  bool          // at least one saved network is visible in the last scan
	Paired        bool          // WhatsApp is linked
	EthernetUp    bool          // an ethernet interface has an address
	ApClients     int           // stations on the setup AP
	ConnectFailed bool          // the last client connect attempt timed out
	LostFor       time.Duration // how long the connection has been gone (0 = fine)
	NoStationsFor time.Duration // how long the AP has had no clients
	Policy        APPolicy
	APAvailable   bool // the interface can host an AP
}

// decision is the outcome of the policy for one step.
type decision struct {
	State  wifiState
	Action action
	After  time.Duration // when to re-evaluate (with actSchedule)
}

// next is the policy state machine as a pure function: given the current
// state and the inputs it returns the next state and the action the loop must
// perform to get there.
func next(cur wifiState, in inputs) decision {
	// "always" and "never" are absolute where the AP is involved.
	if in.Policy == APAlways && in.APAvailable {
		if cur != stateAP {
			return decision{State: stateAP, Action: actStartAP}
		}
		return decision{State: cur}
	}

	switch cur {
	case stateOff:
		return fromOff(in)
	case stateConnected:
		if in.LostFor >= lostRetry {
			return decision{State: stateConnecting, Action: actConnect}
		}
		return decision{State: cur}
	case stateConnecting:
		if in.ConnectFailed {
			return connectFailed(in)
		}
		if in.Saved == 0 {
			return fromOff(in)
		}
		return decision{State: cur}
	case stateAP:
		if in.Policy == APNever {
			return decision{State: stateOff, Action: actStopAP}
		}
		// Go back to client mode when nobody uses the AP, networks are saved
		// and one of them is visible.
		if in.ApClients == 0 && in.NoStationsFor >= noStationsScan &&
			in.Saved > 0 && in.SavedVisible {
			return decision{State: stateConnecting, Action: actStopAP}
		}
		return decision{State: cur}
	}
	return decision{State: cur}
}

// fromOff decides what to do when nothing is running yet.
func fromOff(in inputs) decision {
	if in.Saved > 0 {
		return decision{State: stateConnecting, Action: actConnect}
	}
	if in.Policy == APNever || !in.APAvailable {
		return decision{State: stateOff}
	}
	if !in.Paired || !in.EthernetUp {
		return decision{State: stateAP, Action: actStartAP}
	}
	// Paired and reachable over ethernet: stay off now, try again later.
	return decision{State: stateOff, Action: actSchedule, After: connectRetry}
}

// connectFailed decides between the AP and another attempt after a failed
// client connection.
func connectFailed(in inputs) decision {
	if in.Policy == APNever || !in.APAvailable {
		return decision{State: stateConnecting, Action: actConnect}
	}
	if !in.Paired || !in.EthernetUp {
		return decision{State: stateAP, Action: actStartAP}
	}
	return decision{State: stateOff, Action: actSchedule, After: connectRetry}
}

// String makes failures readable in tests.
func (d decision) String() string {
	return fmt.Sprintf("state=%s action=%s after=%s", d.State, d.Action, d.After)
}
