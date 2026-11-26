//go:build linux

package sntp

import (
	"fmt"
	"time"

	"golang.org/x/sys/unix"
)

// setSystemClock steps CLOCK_REALTIME to t and clears STA_UNSYNC in the kernel
// clock status so netmgr.NTPSynced immediately reports the clock synchronised.
// Maxerror and Esterror are also reset to zero so the kernel's second_overflow
// handler does not re-assert STA_UNSYNC one second later.
func setSystemClock(t time.Time) error {
	ts := unix.NsecToTimespec(t.UnixNano())
	if err := unix.ClockSettime(unix.CLOCK_REALTIME, &ts); err != nil {
		return fmt.Errorf("clock_settime: %w", err)
	}

	var tx unix.Timex
	if _, err := unix.Adjtimex(&tx); err != nil {
		return fmt.Errorf("adjtimex read: %w", err)
	}
	tx = unix.Timex{
		Modes:    unix.ADJ_STATUS | unix.ADJ_MAXERROR | unix.ADJ_ESTERROR,
		Status:   tx.Status &^ unix.STA_UNSYNC,
		Maxerror: 0,
		Esterror: 0,
	}
	if _, err := unix.Adjtimex(&tx); err != nil {
		return fmt.Errorf("adjtimex clear STA_UNSYNC: %w", err)
	}
	return nil
}
