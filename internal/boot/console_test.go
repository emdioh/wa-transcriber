package boot

import (
	"bytes"
	"context"
	"errors"
	"io"
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
		// A typo must not open a console unintentionally.
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
			t.Errorf("spawned a console on %q, which wat.txt did not ask for", tty)
		}
		delete(want, tty)
	}
	if len(want) != 0 {
		t.Fatalf("no console on %v (spawned: %v)", keys(want), spawned)
	}

	// The gadget tree created through configfs.
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
			t.Errorf("wat.txt %q spawned a console on %v", body, spawned)
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

func TestSerialDevice(t *testing.T) {
	tests := []struct {
		model string
		want  string
	}{
		{"TI AM335x BeagleBone Black", "ttyS0"},
		{"Cubietech Cubieboard", "ttyS0"},
		{"Raspberry Pi 2 Model B Rev 1.1", "ttyAMA0"},
	}
	for _, tc := range tests {
		m := newFakeMachine(t)
		o := m.options()
		o.ModelFile = filepath.Join(m.dir, "model")
		writeFile(t, o.ModelFile, []byte(tc.model+"\x00"))
		got, err := o.serialDevice()
		if err != nil {
			t.Errorf("serialDevice(%q): %v", tc.model, err)
			continue
		}
		if got != tc.want {
			t.Errorf("serialDevice(%q) = %q, want %q", tc.model, got, tc.want)
		}
	}
}

type bufReadWriter struct {
	io.Reader
	io.Writer
}

func TestServeConsoleCommands(t *testing.T) {
	m := newFakeMachine(t)
	o := m.options().withDefaults()

	writeFile(t, filepath.Join(o.ProcDir, "uptime"), []byte("1234.56 2345.67\n"))
	writeFile(t, filepath.Join(o.ProcDir, "meminfo"), []byte(
		"MemTotal:        512000 kB\n"+
			"MemFree:         200000 kB\n"+
			"MemAvailable:    350000 kB\n"+
			"Buffers:          10000 kB\n"+
			"Cached:          140000 kB\n"+
			"SwapCached:           0 kB\n"+
			"Active:          100000 kB\n"))
	writeFile(t, filepath.Join(o.ProcDir, "1", "comm"), []byte("wa-transcriber\n"))
	writeFile(t, filepath.Join(o.ProcDir, "42", "comm"), []byte("wpa_supplicant\n"))
	writeFile(t, filepath.Join(o.ProcDir, "7", "cmdline"), []byte("/usr/sbin/hostapd\x00-B\x00"))
	writeFile(t, filepath.Join(o.ProcDir, "self", "comm"), []byte("ignored\n"))

	var rebootCalls int
	o.Reboot = func() error {
		rebootCalls++
		return nil
	}

	input := strings.Join([]string{
		"",
		"help",
		"uptime",
		"mem",
		"mounts",
		"ip",
		"ps",
		"bogus",
		"reboot",
		"help", // must not run after successful reboot
	}, "\n") + "\n"
	var out bytes.Buffer
	rw := bufReadWriter{Reader: strings.NewReader(input), Writer: &out}

	o.serveConsole(context.Background(), rw, o.Log)

	got := out.String()
	for _, want := range []string{
		"wa-transcriber diagnostic console (type 'help')",
		"wat> ",
		"uptime  show system uptime",
		"1234.56 2345.67",
		"MemTotal:        512000 kB",
		"SwapCached:           0 kB",
		"/dev/root",
		"     1 wa-transcriber",
		"     7 hostapd",
		"    42 wpa_supplicant",
		`unknown command "bogus" (type 'help')`,
		"rebooting...",
	} {
		if !strings.Contains(got, want) {
			t.Errorf("console output missing %q:\n%s", want, got)
		}
	}
	// mem prints only the first 6 lines of /proc/meminfo.
	if strings.Contains(got, "Active:          100000 kB") {
		t.Errorf("mem output should be capped at 6 lines:\n%s", got)
	}
	// ps must list PIDs in numeric order (1, 7, 42) and ignore non-numeric entries.
	i1 := strings.Index(got, "     1 wa-transcriber")
	i7 := strings.Index(got, "     7 hostapd")
	i42 := strings.Index(got, "    42 wpa_supplicant")
	if !(i1 >= 0 && i1 < i7 && i7 < i42) {
		t.Errorf("ps output not sorted numerically (1=%d, 7=%d, 42=%d):\n%s", i1, i7, i42, got)
	}
	if strings.Contains(got, "ignored") {
		t.Errorf("ps output included non-numeric proc entry:\n%s", got)
	}
	// ip should print at least one interface line (e.g. lo) on the test host.
	if !strings.Contains(got, "lo (") && !strings.Contains(got, "127.0.0.1") {
		t.Errorf("ip output did not include loopback interface:\n%s", got)
	}
	if rebootCalls != 1 {
		t.Errorf("rebootCalls = %d, want 1", rebootCalls)
	}
}

func TestServeConsoleRebootFailureContinues(t *testing.T) {
	m := newFakeMachine(t)
	o := m.options().withDefaults()
	o.Reboot = func() error { return errors.New("operation not permitted") }

	var out bytes.Buffer
	rw := bufReadWriter{
		Reader: strings.NewReader("reboot\nhelp\n"),
		Writer: &out,
	}
	o.serveConsole(context.Background(), rw, o.Log)

	got := out.String()
	if !strings.Contains(got, "reboot failed: operation not permitted") {
		t.Errorf("expected reboot error in output:\n%s", got)
	}
	if !strings.Contains(got, "commands:") {
		t.Errorf("expected console to stay alive after failed reboot:\n%s", got)
	}
}

func TestClaimConsoleDeduplicatesInProcess(t *testing.T) {
	const tty = "ttyTestDedup0"
	t.Cleanup(func() { releaseConsole(tty) })
	if !claimConsole(tty) {
		t.Fatal("first claimConsole returned false")
	}
	if claimConsole(tty) {
		t.Fatal("second claimConsole returned true while still active")
	}
	releaseConsole(tty)
	if !claimConsole(tty) {
		t.Fatal("claimConsole after release returned false")
	}
}

func keys(m map[string]bool) []string {
	out := make([]string, 0, len(m))
	for k := range m {
		out = append(out, k)
	}
	return out
}
