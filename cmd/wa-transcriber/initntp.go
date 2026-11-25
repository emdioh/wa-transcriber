// The time daemon, for the boards that boot with init= (D19).
//
// The image used to start busybox ntpd from /etc/init.d/S40ntpd. With no
// inittab nothing does, and WhatsApp will not connect with a 1970 clock
// (there is no RTC battery on these boards, so every boot starts at the epoch
// until something asks the network). Until internal/sntp replaces it, this is
// the one daemon of the boot path that is not the program itself.
package main

import (
	"context"
	"log/slog"
	"os/exec"

	"github.com/emdioh/wa-transcriber/internal/proc"
)

// startNTP starts the time daemon and returns immediately. The commands match
// S40ntpd: two servers, so one DNS failure does not leave the device without
// time. It is started after the network is up (nf_conntrack and DNS are
// needed) and stopped by the context.
func startNTP(ctx context.Context, log *slog.Logger) {
	cmd := exec.CommandContext(ctx, "ntpd", "-p", "pool.ntp.org", "-p", "time.cloudflare.com")
	if err := proc.Start(cmd); err != nil {
		log.Warn("cannot start the time daemon, the clock will stay wrong", "err", err)
		return
	}
	log.Info("time daemon started", "program", "ntpd")
	go func() {
		if err := proc.Wait(cmd); err != nil && ctx.Err() == nil {
			log.Warn("the time daemon stopped", "err", err)
		}
	}()
}
