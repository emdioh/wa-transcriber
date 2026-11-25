package boot

import (
	"context"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"time"

	"github.com/emdioh/wa-transcriber/internal/proc"
)

// The opt-in debug shell.
//
// It used to be /etc/init.d/S02console, started from rcS. With no inittab it
// has to be started here, and it matters more than it looks: the USB CDC-ACM
// console is the only way onto a board that has no keyboard and no other
// network, and it exists only while a userspace program creates it. Booting as
// init without this step would take the console away exactly when it is
// needed most — after a change to the boot path.
//
// The shell itself is busybox getty/login: the one place where the init path
// still starts something it does not implement, on purpose. It is off unless
// wat.txt on the boot partition says otherwise.

// consoleChannels is the console= line of wat.txt.
type consoleChannels struct {
	serial bool
	usb    bool
	hdmi   bool
}

// parseConsoleMode reads a console= value. An empty value is off, and an
// unknown one is off as well: a typo must not open a passwordless root shell.
func parseConsoleMode(v string) (consoleChannels, bool) {
	switch v {
	case "off", "":
		return consoleChannels{}, true
	case "serial":
		return consoleChannels{serial: true}, true
	case "usb":
		return consoleChannels{usb: true}, true
	case "hdmi":
		return consoleChannels{hdmi: true}, true
	case "both":
		return consoleChannels{serial: true, usb: true}, true
	case "all":
		return consoleChannels{serial: true, usb: true, hdmi: true}, true
	}
	return consoleChannels{}, false
}

// watValue is the last value of key=value in a wat.txt body. The file lives on
// a FAT partition and is edited on other machines, so Windows line endings,
// comments and spaces around the '=' are tolerated.
func watValue(data []byte, key string) (string, bool) {
	value, found := "", false
	for _, line := range strings.Split(string(data), "\n") {
		line = strings.TrimRight(line, "\r")
		if i := strings.IndexByte(line, '#'); i >= 0 {
			line = line[:i]
		}
		name, v, ok := strings.Cut(line, "=")
		if !ok || strings.TrimSpace(name) != key {
			continue
		}
		value, found = strings.TrimSpace(v), true
	}
	return value, found
}

// setupConsole starts the login shell the boot partition asks for.
func (o Options) setupConsole(ctx context.Context) {
	b, err := o.ReadFile(o.WatFile)
	if err != nil {
		return // no wat.txt: no console, which is the safe default
	}
	value, _ := watValue(b, "console")
	ch, ok := parseConsoleMode(value)
	if !ok {
		o.Log.Warn("unknown console= in wat.txt, the debug shell stays off",
			"value", value, "want", "off, serial, usb, hdmi, both or all")
		return
	}
	if !ch.serial && !ch.usb && !ch.hdmi {
		return
	}
	o.Log.Warn("debug console enabled: it is a root shell without a password",
		"serial", ch.serial, "usb", ch.usb, "hdmi", ch.hdmi)

	if ch.serial {
		if dev, err := o.serialDevice(); err != nil {
			o.Log.Warn("no serial console", "err", err)
		} else {
			o.spawn(ctx, dev)
		}
	}
	if ch.usb {
		if err := o.usbACM(ctx); err != nil {
			o.Log.Warn("no USB console", "err", err)
		}
	}
	if ch.hdmi {
		if _, err := os.Stat("/dev/tty1"); err != nil {
			o.Log.Warn("no HDMI console, there is no /dev/tty1")
		} else {
			o.spawn(ctx, "tty1")
		}
	}
}

// serialDevice names the UART header by the board's model string: the
// BeagleBone uses 8250_omap (ttyS0, ttyO0 before Linux 4.11), the Raspberry Pi
// 2 exposes the PL011 as ttyAMA0 (its ttyS0 is the mini UART on other pins).
func (o Options) serialDevice() (string, error) {
	model, _ := o.ReadFile(o.ModelFile)
	if strings.Contains(string(model), "BeagleBone") {
		return "ttyS0", nil
	}
	if strings.Contains(string(model), "Raspberry") {
		return "ttyAMA0", nil
	}
	for _, dev := range []string{"ttyS0", "ttyAMA0"} {
		if _, err := os.Stat(filepath.Join(o.DevDir, dev)); err == nil {
			return dev, nil
		}
	}
	return "", fmt.Errorf("boot: no serial device for the console")
}

// spawn runs a getty on tty in a respawn loop, like an init would: a login
// that ends, or a tty that is not ready yet, gets a new one a second later.
func (o Options) spawn(ctx context.Context, tty string) {
	if o.Spawn != nil {
		o.Spawn(tty)
		return
	}
	go func() {
		for ctx.Err() == nil {
			cmd := exec.CommandContext(ctx, "getty", "-L", "115200", tty, "vt100")
			if err := proc.Start(cmd); err != nil {
				o.Log.Warn("cannot start the console login", "tty", tty, "err", err)
				return
			}
			_ = proc.Wait(cmd)
			o.Log.Info("console login ended, restarting it", "tty", tty)
			select {
			case <-ctx.Done():
				return
			case <-time.After(time.Second):
			}
		}
	}()
}

// usbACM exposes a CDC-ACM port on the USB client port through configfs (the
// kernel has CONFIG_USB_CONFIGFS_ACM=y, so no module is needed) and logs in on
// the /dev/ttyGS0 it creates. The attribute values are the ones S02console
// wrote: Linux Foundation vendor id, multifunction composite gadget.
func (o Options) usbACM(ctx context.Context) error {
	if err := o.ensureMount(mountSpec{source: "configfs", target: o.ConfigFS, fstype: "configfs"}); err != nil {
		return err
	}
	g := filepath.Join(o.ConfigFS, "usb_gadget", "wat")
	if _, err := os.Stat(g); err != nil {
		attrs := map[string]string{
			"idVendor":  "0x1d6b",
			"idProduct": "0x0104",
			"bcdUSB":    "0x0100",
			"bcdDevice": "0x0100",
		}
		dirs := []string{
			"strings/0x409",
			"configs/c.1/strings/0x409",
			"functions/acm.usb0",
		}
		for _, d := range dirs {
			if err := os.MkdirAll(filepath.Join(g, d), 0o755); err != nil {
				return fmt.Errorf("boot: usb gadget %s: %w", d, err)
			}
		}
		text := map[string]string{
			"strings/0x409/serialnumber":              "wa-transcriber",
			"strings/0x409/manufacturer":              "wa",
			"strings/0x409/product":                   "wa-transcriber console",
			"configs/c.1/strings/0x409/configuration": "acm",
		}
		for name, value := range attrs {
			if err := os.WriteFile(filepath.Join(g, name), []byte(value), 0o644); err != nil {
				return fmt.Errorf("boot: usb gadget %s: %w", name, err)
			}
		}
		for name, value := range text {
			if err := os.WriteFile(filepath.Join(g, name), []byte(value), 0o644); err != nil {
				return fmt.Errorf("boot: usb gadget %s: %w", name, err)
			}
		}
		if err := os.Symlink(filepath.Join(g, "functions/acm.usb0"), filepath.Join(g, "configs/c.1/acm.usb0")); err != nil && !os.IsExist(err) {
			return fmt.Errorf("boot: usb gadget function link: %w", err)
		}
	}
	udc, err := o.firstUDC()
	if err != nil {
		return err
	}
	if err := os.WriteFile(filepath.Join(g, "UDC"), []byte(udc), 0o644); err != nil {
		return fmt.Errorf("boot: usb gadget UDC: %w", err)
	}
	deadline := time.Now().Add(o.Wait)
	for {
		if _, err := os.Stat(filepath.Join(o.DevDir, "ttyGS0")); err == nil {
			o.spawn(ctx, "ttyGS0")
			return nil
		}
		if time.Now().After(deadline) {
			return fmt.Errorf("boot: /dev/ttyGS0 did not appear after binding %s", udc)
		}
		time.Sleep(200 * time.Millisecond)
	}
}

// firstUDC returns the USB device controller to bind the gadget to.
func (o Options) firstUDC() (string, error) {
	ents, err := os.ReadDir(o.UDCDir)
	if err != nil {
		return "", fmt.Errorf("boot: %s: %w", o.UDCDir, err)
	}
	for _, e := range ents {
		return e.Name(), nil
	}
	return "", fmt.Errorf("boot: no USB device controller, is the port in peripheral mode?")
}
