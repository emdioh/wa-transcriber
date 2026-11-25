//go:build !linux

package boot

import (
	"log/slog"
	"os"
)

// Crash cannot reboot a machine it is not the init of; it reports and exits,
// which is what a developer's shell expects.
func Crash(log *slog.Logger, err error) {
	if log != nil {
		log.Error("fatal error", "err", err)
	}
	os.Exit(1)
}
