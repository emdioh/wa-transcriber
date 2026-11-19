package netmgr

import (
	"context"
	"log/slog"
	"time"
)

// ClockWait is how long the binary waits for a sane clock before connecting
// to WhatsApp anyway (PLAN.md M5 step 10).
const ClockWait = 2 * time.Minute

// ClockCheck is the subsystem the clock wait depends on; tests replace it.
type ClockCheck struct {
	// Managed enables the NTP-synced requirement (device image).
	Managed bool
	// Now returns the current time.
	Now func() time.Time
	// BuildTime is the binary's build time.
	BuildTime time.Time
	// Synced reports whether the kernel clock is NTP-synced. Only used in
	// managed mode; nil means "assume synced".
	Synced func() bool
	// Poll is the check interval.
	Poll time.Duration
	Log  *slog.Logger
}

// WaitClock blocks until the clock is sane (later than the build time and,
// in managed mode, NTP-synced) or ClockWait elapses. It reports whether the
// clock was sane when it returned.
func WaitClock(ctx context.Context, c ClockCheck) bool {
	if c.Poll == 0 {
		c.Poll = 2 * time.Second
	}
	if c.Log == nil {
		c.Log = slog.Default()
	}
	deadline := time.Now().Add(ClockWait)
	for {
		if clockSane(c) {
			return true
		}
		if time.Now().After(deadline) {
			c.Log.Warn("clock is still not synchronised, connecting anyway")
			return false
		}
		t := time.NewTimer(c.Poll)
		select {
		case <-ctx.Done():
			t.Stop()
			return false
		case <-t.C:
		}
	}
}

func clockSane(c ClockCheck) bool {
	if c.Now == nil {
		return true
	}
	if !c.BuildTime.IsZero() && !c.Now().After(c.BuildTime) {
		return false
	}
	if c.Managed && c.Synced != nil && !c.Synced() {
		return false
	}
	return true
}
