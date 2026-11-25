// Init mode (D19): the kernel starts this binary as the machine's init, so
// there is no /etc/inittab to carry the arguments the image used to pass
// there. The board's settings are the defaults below and the kernel command
// line may override every one of them as wat.<flag>=<value>.

package main

import (
	"flag"
	"fmt"
	"log/slog"
	"os"
	"sort"
	"strings"

	"github.com/emdioh/wa-transcriber/internal/boot"
)

// initDefaults are the flags /etc/inittab passed to the transcriber. They are
// only applied to a flag that was not given on the command line, so a board
// that needs something else can still say so.
var initDefaults = map[string]string{
	"data":         boot.DataDir,
	"listen":       ":80",
	"network":      "managed",
	"watchdog":     "/dev/watchdog",
	"update-dir":   "/data/bin",
	"image-update": "/data",
	"bootstrap":    "/usr/bin/wa-transcriber",
}

// initPath is the PATH the kernel does not give init: it passes a private
// environment with HOME and TERM and nothing else, and every helper the boot
// path and the network manager run (getty, modprobe, sfdisk, partx, mkfs.ext4,
// e2fsck, ntpd, udhcpc, wpa_supplicant) lives in one of these directories.
// Without it every exec by bare name fails with "executable file not found".
const initPath = "/sbin:/usr/sbin:/bin:/usr/bin"

// setInitEnv fills the environment the kernel does not pass to init.
func setInitEnv(lookupEnv func(string) (string, bool), setenv func(string, string) error) {
	if _, ok := lookupEnv("PATH"); ok {
		return
	}
	_ = setenv("PATH", initPath)
}

// initFallback hands the machine back to the old init path when init mode
// cannot come up: /sbin/init (busybox) reads /etc/inittab, runs rcS and
// respawns the transcriber exactly as before D19.
//
// This is what makes the mode safe to try on a board that is twenty minutes
// away: a mount that does not work out leaves a device that boots the old way
// with a console, instead of a reboot loop whose only exit is a card reader.
// Everything Setup does is idempotent, so the old scripts find their
// filesystems where it left them.
func initFallback(log *slog.Logger, cause error) {
	log.Error("init mode failed, falling back to /sbin/init", "err", cause)
	if f, err := os.OpenFile("/dev/kmsg", os.O_WRONLY, 0); err == nil {
		fmt.Fprintf(f, "wa-transcriber: init mode failed (%v), starting /sbin/init\n", cause)
		_ = f.Close()
	}
	if err := execFunc("/sbin/init", []string{"/sbin/init"}, os.Environ()); err != nil {
		// No busybox either: the only thing left is the crash path.
		boot.Crash(log, fmt.Errorf("init mode failed (%v) and /sbin/init failed: %w", cause, err))
	}
}

// watchdogModules are tried in order: the driver differs per board (BCM2835
// for the Raspberry Pies, OMAP for the BeagleBone) and only the first one
// that loads matters.
var watchdogModules = []string{"bcm2835_wdt", "omap_wdt"}

// readKernelCmdline returns the kernel command line ("" when unreadable).
func readKernelCmdline() string {
	b, err := os.ReadFile("/proc/cmdline")
	if err != nil {
		return ""
	}
	return strings.TrimSpace(string(b))
}

// watArgs returns the wat.<flag>=<value> pairs of a kernel command line.
func watArgs(cmdline string) map[string]string {
	out := make(map[string]string)
	for _, arg := range strings.Fields(cmdline) {
		v, ok := strings.CutPrefix(arg, "wat.")
		if !ok {
			continue
		}
		name, value, ok := strings.Cut(v, "=")
		if !ok || name == "" {
			continue
		}
		out[name] = value
	}
	return out
}

// envName is the environment variable a flag reads: WAT_LISTEN for -listen.
// Every flag this touches follows that spelling, which is what makes the
// environment a reliable "somebody set this on purpose" signal.
func envName(flagName string) string {
	return "WAT_" + strings.ToUpper(strings.ReplaceAll(flagName, "-", "_"))
}

// applyInitDefaults fills the flags that were neither given on the command
// line nor set in the environment: the kernel command line first and
// initDefaults second. It returns the settings it applied, sorted, for the
// boot log (names and values, never a secret twice: these flags carry none).
func applyInitDefaults(fs *flag.FlagSet, cmdline string, lookupEnv func(string) (string, bool)) ([]string, error) {
	given := make(map[string]bool)
	fs.Visit(func(f *flag.Flag) { given[f.Name] = true })
	args := watArgs(cmdline)

	var applied []string
	var firstErr error
	fs.VisitAll(func(f *flag.Flag) {
		if given[f.Name] || firstErr != nil {
			return
		}
		// The environment is a setting too: the container image sets
		// WAT_LISTEN and a board must not move the web UI because of a
		// default.
		if _, ok := lookupEnv(envName(f.Name)); ok {
			return
		}
		value, ok := args[f.Name]
		if !ok {
			value, ok = initDefaults[f.Name]
		}
		if !ok {
			return
		}
		if err := fs.Set(f.Name, value); err != nil {
			firstErr = fmt.Errorf("kernel command line wat.%s: %w", f.Name, err)
			return
		}
		applied = append(applied, f.Name+"="+f.Value.String())
	})
	sort.Strings(applied)
	return applied, firstErr
}
