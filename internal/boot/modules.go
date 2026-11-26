package boot

import (
	"context"
	"os"
	"path/filepath"
	"strings"
)

// USB kernel module autoloading for init mode.
//
// The board kernels build the USB WiFi drivers (rt2800usb, mt76x2u, rtl8xxxu,
// cfg80211, mac80211) as modules. Under busybox init, Buildroot's S10mdev
// registers /sbin/mdev as the kernel hotplug helper and scans sysfs to load
// modules for devices already attached at boot. In init mode nothing runs
// S10mdev, so boot.Setup has to do both jobs itself.

// coldplugUSB reads every non-empty modalias file under
// <SysDir>/bus/usb/devices/*/modalias, deduplicates the aliases, and asks
// modprobe to load the matching kernel module for each. Failures are ignored:
// hubs, built-in drivers, and devices without a matching module are normal.
func (o Options) coldplugUSB(ctx context.Context) {
	pattern := filepath.Join(o.SysDir, "bus", "usb", "devices", "*", "modalias")
	paths, err := filepath.Glob(pattern)
	if err != nil {
		o.Log.Warn("cannot scan USB modalias files", "pattern", pattern, "err", err)
		return
	}
	seen := make(map[string]struct{}, len(paths))
	for _, p := range paths {
		if ctx.Err() != nil {
			return
		}
		b, err := o.ReadFile(p)
		if err != nil {
			o.Log.Debug("cannot read USB modalias", "path", p, "err", err)
			continue
		}
		alias := strings.TrimSpace(string(b))
		if alias == "" {
			continue
		}
		if _, dup := seen[alias]; dup {
			continue
		}
		seen[alias] = struct{}{}
		if err := o.Run(ctx, "", "modprobe", "-b", "-q", alias); err != nil {
			o.Log.Debug("USB modalias not loaded", "modalias", alias, "err", err)
			continue
		}
		o.Log.Info("USB kernel module probed", "modalias", alias)
	}
}

// registerHotplug writes Options.HotplugHelper (default /sbin/mdev) to
// <ProcDir>/sys/kernel/hotplug when both exist, so USB dongles plugged in
// after boot also get their module loaded via MODALIAS. Best-effort: kernels
// without CONFIG_UEVENT_HELPER do not expose the sysctl file.
func (o Options) registerHotplug() {
	if o.HotplugHelper == "" {
		return
	}
	info, err := os.Stat(o.HotplugHelper)
	if err != nil || info.IsDir() {
		o.Log.Debug("hotplug helper not available", "helper", o.HotplugHelper)
		return
	}
	hotplugPath := filepath.Join(o.ProcDir, "sys", "kernel", "hotplug")
	if _, err := os.Stat(hotplugPath); err != nil {
		o.Log.Debug("kernel hotplug sysctl not available", "path", hotplugPath, "err", err)
		return
	}
	f, err := os.OpenFile(hotplugPath, os.O_WRONLY|os.O_TRUNC, 0)
	if err != nil {
		o.Log.Warn("cannot open kernel hotplug sysctl", "path", hotplugPath, "err", err)
		return
	}
	_, writeErr := f.WriteString(o.HotplugHelper + "\n")
	closeErr := f.Close()
	if writeErr != nil {
		o.Log.Warn("cannot register hotplug helper", "path", hotplugPath, "helper", o.HotplugHelper, "err", writeErr)
		return
	}
	if closeErr != nil {
		o.Log.Warn("cannot close kernel hotplug sysctl", "path", hotplugPath, "helper", o.HotplugHelper, "err", closeErr)
		return
	}
	o.Log.Info("hotplug helper registered", "helper", o.HotplugHelper)
}
