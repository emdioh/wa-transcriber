//go:build linux

package boot

import (
	"fmt"
	"log/slog"
	"os"
	"time"

	"golang.org/x/sys/unix"
)

// Crash is what a fatal error does as init.
//
// PID 1 cannot simply exit: the kernel panics with "Attempted to kill init",
// which prints a traceback on the console and writes nothing anywhere useful.
// A device with no keyboard has to come back on its own, so the message goes
// to /dev/kmsg (the kernel prints it on every active console) and the machine
// reboots. If the reboot call is refused the process blocks here with the
// watchdog still open and unpetted: the hardware resets within ~20 s.
//
// It never returns.
func Crash(log *slog.Logger, err error) {
	if log != nil {
		log.Error("fatal error, rebooting", "err", err)
	}
	if f, ferr := os.OpenFile("/dev/kmsg", os.O_WRONLY, 0); ferr == nil {
		fmt.Fprintf(f, "wa-transcriber: fatal: %v (rebooting)\n", err)
		_ = f.Close()
	}
	unix.Sync()
	if rerr := unix.Reboot(unix.LINUX_REBOOT_CMD_RESTART); rerr != nil {
		if log != nil {
			log.Error("reboot failed, waiting for the watchdog", "err", rerr)
		}
	}
	// Not "select {}": with no other goroutine left the runtime calls that a
	// deadlock and kills the process, which is the one thing an init must not
	// do — it would be a kernel panic instead of a watchdog reset. A sleeping
	// loop keeps the runtime happy and the process alive until the watchdog
	// expires.
	for {
		time.Sleep(time.Hour)
	}
}
