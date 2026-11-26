package netmgr

import (
	"golang.org/x/sys/unix"
)

// NTPSynced reports whether the kernel considers the clock synchronised
// (STA_UNSYNC cleared). It reads the clock state and changes nothing.
func NTPSynced() bool {
	var tx unix.Timex
	state, err := unix.Adjtimex(&tx)
	if err != nil {
		return false
	}
	return state != unix.TIME_ERROR && tx.Status&unix.STA_UNSYNC == 0
}
