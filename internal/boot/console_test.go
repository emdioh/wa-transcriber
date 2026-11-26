package boot

import (
	"context"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

func TestParseConsoleMode(t *testing.T) {
	tests := []struct {
		value string
		want  consoleChannels
		ok    bool
	}{
		{"", consoleChannels{}, true},
		{"off", consoleChannels{}, true},
		{"serial", consoleChannels{serial: true}, true},
		{"usb", consoleChannels{usb: true}, true},
		{"hdmi", consoleChannels{hdmi: true}, true},
		{"both", consoleChannels{serial: true, usb: true}, true},
		{"all", consoleChannels{serial: true, usb: true, hdmi: true}, true},
		// A typo must not open a passwordless root shell.
		{"on", consoleChannels{}, false},
		{"ALL", consoleChannels{}, false},
	}
	for _, tc := range tests {
		got, ok := parseConsoleMode(tc.value)
		if ok != tc.ok || got != tc.want {
			t.Errorf("parseConsoleMode(%q) = %+v, %v; want %+v, %v", tc.value, got, ok, tc.want, tc.ok)
		}
	}
}

func TestWatValue(t *testing.T) {
	body := []byte("# console=usb\r\nconsole = serial  # the header\r\nconsole=all\r\n")
	if v, ok := watValue(body, "console"); !ok || v != "all" {
		t.Fatalf("watValue = %q, %v; want the last value, all", v, ok)
	}
	if _, ok := watValue(body, "country"); ok {
		t.Fatal("watValue found a key that is not there")
	}
	if v, _ := watValue([]byte("setup_ap_key=a#b=c\n"), "setup_ap_key"); v != "a" {
		t.Fatalf("watValue = %q, want the value up to the comment", v)
	}
}

func TestSetupConsoleSpawnsTheAskedChannels(t *testing.T) {
	m := newFakeMachine(t)
	writeFile(t, filepath.Join(m.devDir, "ttyGS0"), nil) // what the gadget creates
	writeFile(t, filepath.Join(m.devDir, "tty1"), nil)
	writeFile(t, filepath.Join(m.dir, "model"), []byte("TI AM335x BeagleBone Black"))
	udc := filepath.Join(m.dir, "udc")
	writeFile(t, filepath.Join(udc, "musb-hdrc.1.auto"), nil)

	var spawned []string
	o := m.options()
	o.WatFile = filepath.Join(m.dir, "wat.txt")
	o.ModelFile = filepath.Join(m.dir, "model")
	o.ConfigFS = filepath.Join(m.dir, "configfs")
	o.UDCDir = udc
	o.Spawn = func(tty string) { spawned = append(spawned, tty) }
	o.Wait = 100_000_000 // 100 ms

	writeFile(t, o.WatFile, []byte("console=all\n"))
	o.setupConsole(context.Background())

	want := map[string]bool{"ttyS0": true, "ttyGS0": true, "tty1": true}
	for _, tty := range spawned {
		if !want[tty] {
			t.Errorf("spawned a login on %q, which wat.txt did not ask for", tty)
		}
		delete(want, tty)
	}
	if len(want) != 0 {
		t.Fatalf("no login on %v (spawned: %v)", keys(want), spawned)
	}

	// The gadget tree is what S02console used to write through configfs.
	g := filepath.Join(o.ConfigFS, "usb_gadget", "wat")
	if v, err := os.ReadFile(filepath.Join(g, "idVendor")); err != nil || strings.TrimSpace(string(v)) != "0x1d6b" {
		t.Errorf("idVendor = %q, %v", v, err)
	}
	if v, err := os.ReadFile(filepath.Join(g, "UDC")); err != nil || strings.TrimSpace(string(v)) != "musb-hdrc.1.auto" {
		t.Errorf("UDC = %q, %v", v, err)
	}
	if _, err := os.Stat(filepath.Join(g, "configs/c.1/acm.usb0")); err != nil {
		t.Errorf("the acm function is not linked into the configuration: %v", err)
	}
}

func TestSetupConsoleOffAndUnknown(t *testing.T) {
	for _, body := range []string{"console=off\n", "console=nonsense\n", ""} {
		m := newFakeMachine(t)
		var spawned []string
		o := m.options()
		o.WatFile = filepath.Join(m.dir, "wat.txt")
		o.ConfigFS = filepath.Join(m.dir, "configfs")
		o.UDCDir = filepath.Join(m.dir, "udc")
		o.Spawn = func(tty string) { spawned = append(spawned, tty) }
		if body != "" {
			writeFile(t, o.WatFile, []byte(body))
		}
		o.setupConsole(context.Background())
		if len(spawned) != 0 {
			t.Errorf("wat.txt %q spawned a login on %v", body, spawned)
		}
	}
}

func TestSetupConsoleWithoutUDC(t *testing.T) {
	m := newFakeMachine(t)
	writeFile(t, filepath.Join(m.devDir, "ttyGS0"), nil)
	var spawned []string
	o := m.options()
	o.WatFile = filepath.Join(m.dir, "wat.txt")
	o.ModelFile = filepath.Join(m.dir, "model")
	o.ConfigFS = filepath.Join(m.dir, "configfs")
	o.UDCDir = filepath.Join(m.dir, "no-udc")
	o.Spawn = func(tty string) { spawned = append(spawned, tty) }
	writeFile(t, o.WatFile, []byte("console=usb\n"))
	writeFile(t, o.ModelFile, []byte("TI AM335x BeagleBone Black"))

	o.setupConsole(context.Background())
	if len(spawned) != 0 {
		t.Fatalf("a USB console was started with no device controller: %v", spawned)
	}
}

func TestUSBACMSkipsUDCBindWhenAlreadyBound(t *testing.T) {
	m := newFakeMachine(t)
	writeFile(t, filepath.Join(m.devDir, "ttyGS0"), nil)
	udcDir := filepath.Join(m.dir, "udc")
	writeFile(t, filepath.Join(udcDir, "musb-hdrc.1.auto"), nil)

	var spawned []string
	o := m.options()
	o.ConfigFS = filepath.Join(m.dir, "configfs")
	o.UDCDir = udcDir
	o.Spawn = func(tty string) { spawned = append(spawned, tty) }

	// Simulate re-exec: gadget directory already exists and UDC is already bound
	// (with trailing newline as Linux configfs returns it). Mark UDC read-only
	// so any attempt to rewrite it fails.
	udcPath := filepath.Join(o.ConfigFS, "usb_gadget", "wat", "UDC")
	writeFile(t, udcPath, []byte("musb-hdrc.1.auto\n"))
	if err := os.Chmod(udcPath, 0o444); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = os.Chmod(udcPath, 0o644) })

	if err := o.usbACM(context.Background()); err != nil {
		t.Fatalf("usbACM on already-bound gadget failed: %v", err)
	}
	if len(spawned) != 1 || spawned[0] != "ttyGS0" {
		t.Fatalf("spawned = %v, want [ttyGS0]", spawned)
	}
	got, err := os.ReadFile(udcPath)
	if err != nil {
		t.Fatal(err)
	}
	if string(got) != "musb-hdrc.1.auto\n" {
		t.Fatalf("UDC content changed to %q, want untouched %q", got, "musb-hdrc.1.auto\n")
	}
}

func TestUSBACMBindsWhenUDCFileIsEmpty(t *testing.T) {
	m := newFakeMachine(t)
	writeFile(t, filepath.Join(m.devDir, "ttyGS0"), nil)
	udcDir := filepath.Join(m.dir, "udc")
	writeFile(t, filepath.Join(udcDir, "musb-hdrc.1.auto"), nil)

	var spawned []string
	o := m.options()
	o.ConfigFS = filepath.Join(m.dir, "configfs")
	o.UDCDir = udcDir
	o.Spawn = func(tty string) { spawned = append(spawned, tty) }

	// Gadget directory exists but UDC is unbound ("\n" in Linux configfs).
	udcPath := filepath.Join(o.ConfigFS, "usb_gadget", "wat", "UDC")
	writeFile(t, udcPath, []byte("\n"))

	if err := o.usbACM(context.Background()); err != nil {
		t.Fatalf("usbACM on unbound gadget failed: %v", err)
	}
	got, err := os.ReadFile(udcPath)
	if err != nil {
		t.Fatal(err)
	}
	if strings.TrimSpace(string(got)) != "musb-hdrc.1.auto" {
		t.Fatalf("UDC = %q, want musb-hdrc.1.auto", got)
	}
	if len(spawned) != 1 || spawned[0] != "ttyGS0" {
		t.Fatalf("spawned = %v, want [ttyGS0]", spawned)
	}
}

func TestSetupConsoleSkipsAlreadyRunningGetty(t *testing.T) {
	m := newFakeMachine(t)
	writeFile(t, filepath.Join(m.devDir, "ttyGS0"), nil)
	writeFile(t, filepath.Join(m.devDir, "tty1"), nil)
	writeFile(t, filepath.Join(m.dir, "model"), []byte("TI AM335x BeagleBone Black"))
	udcDir := filepath.Join(m.dir, "udc")
	writeFile(t, filepath.Join(udcDir, "musb-hdrc.1.auto"), nil)

	var spawned []string
	o := m.options()
	o.WatFile = filepath.Join(m.dir, "wat.txt")
	o.ModelFile = filepath.Join(m.dir, "model")
	o.ConfigFS = filepath.Join(m.dir, "configfs")
	o.UDCDir = udcDir
	o.Spawn = func(tty string) { spawned = append(spawned, tty) }
	writeFile(t, o.WatFile, []byte("console=all\n"))

	// ttyS0 has a getty, ttyGS0 has a busybox getty with /dev/ path;
	// tty1 has none (tty10 and grep tty1 must not suppress tty1).
	writeFile(t, filepath.Join(o.ProcDir, "100", "cmdline"), []byte("getty\x00-L\x00115200\x00ttyS0\x00vt100\x00"))
	writeFile(t, filepath.Join(o.ProcDir, "101", "cmdline"), []byte("/bin/busybox\x00getty\x00-L\x00115200\x00/dev/ttyGS0\x00vt100\x00"))
	writeFile(t, filepath.Join(o.ProcDir, "102", "cmdline"), []byte("getty\x00-L\x00115200\x00tty10\x00vt100\x00"))
	writeFile(t, filepath.Join(o.ProcDir, "103", "cmdline"), []byte("grep\x00tty1\x00/var/log/messages\x00"))
	writeFile(t, filepath.Join(o.ProcDir, "104", "cmdline"), nil)
	writeFile(t, filepath.Join(o.ProcDir, "self", "cmdline"), []byte("getty\x00-L\x00115200\x00tty1\x00vt100\x00"))

	o.setupConsole(context.Background())

	if len(spawned) != 1 || spawned[0] != "tty1" {
		t.Fatalf("spawned = %v, want only [tty1]", spawned)
	}
}

func TestSetupConsoleSkipsAlreadyRunningLogin(t *testing.T) {
	m := newFakeMachine(t)
	writeFile(t, filepath.Join(m.devDir, "ttyGS0"), nil)
	writeFile(t, filepath.Join(m.dir, "model"), []byte("TI AM335x BeagleBone Black"))
	udcDir := filepath.Join(m.dir, "udc")
	writeFile(t, filepath.Join(udcDir, "musb-hdrc.1.auto"), nil)

	var spawned []string
	o := m.options()
	o.WatFile = filepath.Join(m.dir, "wat.txt")
	o.ModelFile = filepath.Join(m.dir, "model")
	o.ConfigFS = filepath.Join(m.dir, "configfs")
	o.UDCDir = udcDir
	o.Spawn = func(tty string) { spawned = append(spawned, tty) }
	writeFile(t, o.WatFile, []byte("console=both\n"))

	// getty has exec'd login on ttyS0: cmdline no longer names ttyS0, but fd/0 does.
	pidDir := filepath.Join(o.ProcDir, "200")
	writeFile(t, filepath.Join(pidDir, "cmdline"), []byte("/bin/login\x00--\x00root\x00"))
	if err := os.MkdirAll(filepath.Join(pidDir, "fd"), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.Symlink("/dev/ttyS0", filepath.Join(pidDir, "fd", "0")); err != nil {
		t.Fatal(err)
	}

	o.setupConsole(context.Background())

	if len(spawned) != 1 || spawned[0] != "ttyGS0" {
		t.Fatalf("spawned = %v, want only [ttyGS0]", spawned)
	}
}

func TestSetupConsoleProceedsWhenProcDirMissing(t *testing.T) {
	m := newFakeMachine(t)
	writeFile(t, filepath.Join(m.dir, "model"), []byte("TI AM335x BeagleBone Black"))

	var spawned []string
	o := m.options()
	o.ProcDir = filepath.Join(m.dir, "missing-proc")
	o.WatFile = filepath.Join(m.dir, "wat.txt")
	o.ModelFile = filepath.Join(m.dir, "model")
	o.Spawn = func(tty string) { spawned = append(spawned, tty) }
	writeFile(t, o.WatFile, []byte("console=serial\n"))

	o.setupConsole(context.Background())

	if len(spawned) != 1 || spawned[0] != "ttyS0" {
		t.Fatalf("spawned = %v, want [ttyS0]", spawned)
	}
}

func keys(m map[string]bool) []string {
	out := make([]string, 0, len(m))
	for k := range m {
		out = append(out, k)
	}
	return out
}
