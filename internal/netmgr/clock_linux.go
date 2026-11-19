package netmgr

import (
	"golang.org/x/sys/unix"
)

// NTPSynced reports whether the kernel considers the clock synchronised
// (STA_UNSYNC cleared). It reads the clock state and changes nothing.
func NTPSynced() bool {
	state, err := unix.Adjtimex(&unix.Timex{})
	if err != nil {
		return false
	}
	return state&unix.STA_UNSYNC == 0
}
