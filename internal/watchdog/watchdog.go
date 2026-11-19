// Package watchdog keeps a hardware watchdog fed while the process is
// healthy. If the process hangs or is stopped (SIGSTOP), the keepalives stop
// and the board reboots, which is how a headless device recovers without
// anybody pressing a button.
package watchdog

import (
	"context"
	"errors"
	"fmt"
	"log/slog"
	"os"
	"time"

	"golang.org/x/sys/unix"
)

// Interval is how often the device is petted. It must stay well below the
// hardware timeout (15 s on bcm2835_wdt / omap_wdt).
const Interval = 5 * time.Second

// keepAlive is WDIOC_KEEPALIVE: _IOR('W', 5, int).
const keepAlive = 0x80045705

// magicClose tells the driver to disable the timer on close, so a clean
// shutdown does not reboot the board.
const magicClose = 'V'

// Device is the part of a watchdog device the keeper needs.
type Device interface {
	// Keepalive resets the hardware timer.
	Keepalive() error
	// Close releases the device; disarms it where the driver supports it.
	Close() error
}

// linuxDevice talks to the real /dev/watchdog.
type linuxDevice struct{ f *os.File }

func (d *linuxDevice) Keepalive() error {
	return unix.IoctlSetInt(int(d.f.Fd()), keepAlive, 0)
}

func (d *linuxDevice) Close() error {
	if _, err := d.f.Write([]byte{magicClose}); err != nil {
		_ = d.f.Close()
		return err
	}
	return d.f.Close()
}

// Keeper feeds one device.
type Keeper struct {
	Dev      Device
	Interval time.Duration
	Log      *slog.Logger
	// OnError is called for each failed keepalive; nil logs instead.
	OnError func(error)
}

// Open returns a keeper for path. A missing device is not fatal: the caller
// logs it and keeps running (a board without a watchdog driver).
func Open(path string, log *slog.Logger) (*Keeper, error) {
	if path == "" {
		return nil, errors.New("watchdog: disabled")
	}
	f, err := os.OpenFile(path, os.O_WRONLY, 0)
	if err != nil {
		return nil, fmt.Errorf("watchdog: %w", err)
	}
	if log == nil {
		log = slog.Default()
	}
	return &Keeper{Dev: &linuxDevice{f: f}, Interval: Interval, Log: log}, nil
}

// Run pets the device until ctx ends, then closes it (disarming the timer).
func (k *Keeper) Run(ctx context.Context) {
	if k.Interval <= 0 {
		k.Interval = Interval
	}
	if err := k.Dev.Keepalive(); err != nil {
		k.report(err)
	}
	t := time.NewTicker(k.Interval)
	defer t.Stop()
	for {
		select {
		case <-ctx.Done():
			if err := k.Dev.Close(); err != nil {
				k.report(err)
			}
			return
		case <-t.C:
			if err := k.Dev.Keepalive(); err != nil {
				k.report(err)
			}
		}
	}
}

func (k *Keeper) report(err error) {
	if k.OnError != nil {
		k.OnError(err)
		return
	}
	if k.Log != nil {
		k.Log.Warn("watchdog keepalive failed", "err", err)
	}
}
