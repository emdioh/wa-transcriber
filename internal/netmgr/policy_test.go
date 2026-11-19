package netmgr

import (
	"testing"
	"time"
)

// everyState is the state set; the table test must cover each one.
var everyState = []wifiState{stateOff, stateConnecting, stateConnected, stateAP}

// scenario is one row of the table.
type scenario struct {
	name string
	cur  wifiState
	in   inputs
	want decision
}

func base() inputs {
	return inputs{
		Saved: 0, Paired: true, EthernetUp: true,
		Policy: APAuto, APAvailable: true,
	}
}

var policyTable = []scenario{
	// -- off ---------------------------------------------------------------
	{"off: saved networks -> connect", stateOff,
		func() inputs { i := base(); i.Saved = 1; return i }(),
		decision{State: stateConnecting, Action: actConnect}},
	{"off: no saved, unpaired -> ap", stateOff,
		func() inputs { i := base(); i.Paired = false; return i }(),
		decision{State: stateAP, Action: actStartAP}},
	{"off: no saved, no ethernet -> ap", stateOff,
		func() inputs { i := base(); i.EthernetUp = false; return i }(),
		decision{State: stateAP, Action: actStartAP}},
	{"off: no saved, paired + ethernet -> schedule", stateOff, base(),
		decision{State: stateOff, Action: actSchedule, After: connectRetry}},
	{"off: policy never -> off", stateOff,
		func() inputs { i := base(); i.Paired = false; i.Policy = APNever; return i }(),
		decision{State: stateOff}},
	{"off: no AP available -> off", stateOff,
		func() inputs { i := base(); i.Paired = false; i.APAvailable = false; return i }(),
		decision{State: stateOff}},

	// -- connecting --------------------------------------------------------
	{"connecting: still trying", stateConnecting,
		func() inputs { i := base(); i.Saved = 2; return i }(),
		decision{State: stateConnecting}},
	{"connecting: failed, unpaired -> ap", stateConnecting,
		func() inputs { i := base(); i.Saved = 1; i.ConnectFailed = true; i.Paired = false; return i }(),
		decision{State: stateAP, Action: actStartAP}},
	{"connecting: failed, no ethernet -> ap", stateConnecting,
		func() inputs { i := base(); i.Saved = 1; i.ConnectFailed = true; i.EthernetUp = false; return i }(),
		decision{State: stateAP, Action: actStartAP}},
	{"connecting: failed, paired + ethernet -> schedule", stateConnecting,
		func() inputs { i := base(); i.Saved = 1; i.ConnectFailed = true; return i }(),
		decision{State: stateOff, Action: actSchedule, After: connectRetry}},
	{"connecting: failed, policy never -> retry", stateConnecting,
		func() inputs { i := base(); i.Saved = 1; i.ConnectFailed = true; i.Policy = APNever; return i }(),
		decision{State: stateConnecting, Action: actConnect}},
	{"connecting: no saved networks -> off rules", stateConnecting, base(),
		decision{State: stateOff, Action: actSchedule, After: connectRetry}},

	// -- connected ---------------------------------------------------------
	{"connected: healthy", stateConnected,
		func() inputs { i := base(); i.Saved = 1; return i }(),
		decision{State: stateConnected}},
	{"connected: lost just now", stateConnected,
		func() inputs { i := base(); i.Saved = 1; i.LostFor = time.Minute; return i }(),
		decision{State: stateConnected}},
	{"connected: lost for 2 min -> reconnect", stateConnected,
		func() inputs { i := base(); i.Saved = 1; i.LostFor = 2 * time.Minute; return i }(),
		decision{State: stateConnecting, Action: actConnect}},

	// -- ap ----------------------------------------------------------------
	{"ap: clients present", stateAP,
		func() inputs { i := base(); i.Saved = 2; i.ApClients = 1; i.NoStationsFor = time.Hour; return i }(),
		decision{State: stateAP}},
	{"ap: empty but not 5 min yet", stateAP,
		func() inputs { i := base(); i.Saved = 2; i.NoStationsFor = 4 * time.Minute; return i }(),
		decision{State: stateAP}},
	{"ap: empty 5 min, saved visible -> connect", stateAP,
		func() inputs {
			i := base()
			i.Saved = 2
			i.SavedVisible = true
			i.NoStationsFor = 5 * time.Minute
			return i
		}(),
		decision{State: stateConnecting, Action: actStopAP}},
	{"ap: empty 5 min, nothing visible -> stay", stateAP,
		func() inputs { i := base(); i.Saved = 2; i.NoStationsFor = 5 * time.Minute; return i }(),
		decision{State: stateAP}},
	{"ap: policy never -> off", stateAP,
		func() inputs { i := base(); i.Policy = APNever; return i }(),
		decision{State: stateOff, Action: actStopAP}},

	// -- policy always -----------------------------------------------------
	{"always: from off -> ap", stateOff,
		func() inputs { i := base(); i.Policy = APAlways; return i }(),
		decision{State: stateAP, Action: actStartAP}},
	{"always: from connected -> ap", stateConnected,
		func() inputs { i := base(); i.Policy = APAlways; return i }(),
		decision{State: stateAP, Action: actStartAP}},
	{"always: already ap", stateAP,
		func() inputs { i := base(); i.Policy = APAlways; return i }(),
		decision{State: stateAP}},
	{"always: no AP available -> unchanged", stateConnected,
		func() inputs { i := base(); i.Policy = APAlways; i.APAvailable = false; return i }(),
		decision{State: stateConnected}},
}

func TestPolicyTable(t *testing.T) {
	for _, s := range policyTable {
		t.Run(s.name, func(t *testing.T) {
			got := next(s.cur, s.in)
			if got != s.want {
				t.Fatalf("next(%s) = %s, want %s", s.cur, got, s.want)
			}
		})
	}
}

// TestPolicyCoversEveryState makes sure a state is never left out of the
// table above.
func TestPolicyCoversEveryState(t *testing.T) {
	for _, st := range everyState {
		found := false
		for _, s := range policyTable {
			if s.cur == st {
				found = true
			}
		}
		if !found {
			t.Errorf("no table row for state %q", st)
		}
	}
}

// TestPolicyIsDeterministic guards against accidental time.Now() usage: the
// same inputs must always produce the same decision.
func TestPolicyIsDeterministic(t *testing.T) {
	for _, s := range policyTable {
		a, b := next(s.cur, s.in), next(s.cur, s.in)
		if a != b {
			t.Errorf("%s: not deterministic: %s vs %s", s.name, a, b)
		}
	}
}
