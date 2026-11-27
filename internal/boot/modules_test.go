package boot

import (
	"context"
	"errors"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"reflect"
	"testing"
)

func TestColdplugUSBLoadsDeduplicatedModaliases(t *testing.T) {
	m := newFakeMachine(t)
	o := m.options().withDefaults()

	usbDir := filepath.Join(m.sysDir, "bus", "usb", "devices")
	// Regular device directory with RT5370 modalias.
	writeFile(t, filepath.Join(usbDir, "1-1:1.0", "modalias"),
		[]byte("usb:v148Fp5370d0101dc00dsc00dp00icFFiscFFipFFin00\n"))
	// Duplicate modalias on another port should only be probed once.
	writeFile(t, filepath.Join(usbDir, "1-2:1.0", "modalias"),
		[]byte("  usb:v148Fp5370d0101dc00dsc00dp00icFFiscFFipFFin00 \n"))
	// Empty and whitespace-only modalias files must be skipped.
	writeFile(t, filepath.Join(usbDir, "1-3:1.0", "modalias"), []byte(""))
	writeFile(t, filepath.Join(usbDir, "1-4:1.0", "modalias"), []byte(" \t\n "))
	// Real sysfs exposes USB interfaces via symlinks into /sys/devices/...
	realDev := filepath.Join(m.sysDir, "devices", "platform", "usb", "1-5:1.0")
	writeFile(t, filepath.Join(realDev, "modalias"),
		[]byte("usb:v0E8Dp7612d0100dc00dsc00dp00icFFiscFFipFFin00\n"))
	if err := os.Symlink(realDev, filepath.Join(usbDir, "1-5:1.0")); err != nil {
		t.Fatal(err)
	}
	// Device with no modalias file (e.g., parent USB device node) is ignored.
	if err := os.MkdirAll(filepath.Join(usbDir, "usb1"), 0o755); err != nil {
		t.Fatal(err)
	}

	// Simulate a modprobe failure on the first alias and verify the second still runs.
	m.runFunc = func(_, name string, args []string) error {
		if name == "modprobe" && len(args) == 3 && args[2] == "usb:v148Fp5370d0101dc00dsc00dp00icFFiscFFipFFin00" {
			return errors.New("exit status 1")
		}
		return nil
	}

	o.coldplugUSB(context.Background())

	want := []string{
		"modprobe -b -q usb:v148Fp5370d0101dc00dsc00dp00icFFiscFFipFFin00",
		"modprobe -b -q usb:v0E8Dp7612d0100dc00dsc00dp00icFFiscFFipFFin00",
	}
	if !reflect.DeepEqual(m.runs, want) {
		t.Fatalf("coldplugUSB runs = %v, want %v", m.runs, want)
	}
}

func TestColdplugUSBMissingSysfsDirIsNoop(t *testing.T) {
	m := newFakeMachine(t)
	o := m.options().withDefaults()

	o.coldplugUSB(context.Background())

	if len(m.runs) != 0 {
		t.Fatalf("expected no modprobe calls when USB sysfs dir is missing, got %v", m.runs)
	}
}

func TestColdplugUSBStopsEarlyWhenModprobeMissing(t *testing.T) {
	m := newFakeMachine(t)
	o := m.options().withDefaults()
	usbDir := filepath.Join(m.sysDir, "bus", "usb", "devices")
	writeFile(t, filepath.Join(usbDir, "1-1:1.0", "modalias"), []byte("usb:v148Fp5370\n"))
	writeFile(t, filepath.Join(usbDir, "1-2:1.0", "modalias"), []byte("usb:v0E8Dp7612\n"))
	m.runFunc = func(_, _ string, _ []string) error {
		return fmt.Errorf("modprobe: %w", exec.ErrNotFound)
	}

	o.coldplugUSB(context.Background())

	if len(m.runs) != 1 {
		t.Fatalf("expected coldplugUSB to stop on first ErrNotFound, got %v", m.runs)
	}
}

func TestRegisterHotplugWritesHelperWhenBothExist(t *testing.T) {
	m := newFakeMachine(t)
	o := m.options().withDefaults()

	helper := filepath.Join(m.dir, "sbin", "mdev")
	writeFile(t, helper, []byte("#!/bin/sh\n"))
	hotplug := filepath.Join(o.ProcDir, "sys", "kernel", "hotplug")
	writeFile(t, hotplug, []byte(""))
	o.HotplugHelper = helper

	o.registerHotplug()

	got := string(mustRead(t, hotplug))
	want := helper + "\n"
	if got != want {
		t.Fatalf("hotplug sysctl = %q, want %q", got, want)
	}
}

func TestRegisterHotplugSkipsWhenHelperOrSysctlMissing(t *testing.T) {
	t.Run("missing helper", func(t *testing.T) {
		m := newFakeMachine(t)
		o := m.options().withDefaults()
		hotplug := filepath.Join(o.ProcDir, "sys", "kernel", "hotplug")
		writeFile(t, hotplug, []byte("original\n"))
		o.HotplugHelper = filepath.Join(m.dir, "sbin", "missing-mdev")

		o.registerHotplug()

		if got := string(mustRead(t, hotplug)); got != "original\n" {
			t.Fatalf("hotplug sysctl modified when helper missing: got %q", got)
		}
	})

	t.Run("missing sysctl", func(t *testing.T) {
		m := newFakeMachine(t)
		o := m.options().withDefaults()
		helper := filepath.Join(m.dir, "sbin", "mdev")
		writeFile(t, helper, []byte("#!/bin/sh\n"))
		o.HotplugHelper = helper

		o.registerHotplug()

		hotplug := filepath.Join(o.ProcDir, "sys", "kernel", "hotplug")
		if _, err := os.Stat(hotplug); !os.IsNotExist(err) {
			t.Fatalf("hotplug sysctl should not be created when absent, stat err = %v", err)
		}
	})

	t.Run("default helper path", func(t *testing.T) {
		if got := (Options{}).withDefaults().HotplugHelper; got != "/sbin/mdev" {
			t.Fatalf("default HotplugHelper = %q, want /sbin/mdev", got)
		}
	})
}

func TestSetupRunsHotplugAndColdplugAlongsideWatchdog(t *testing.T) {
	m := newFakeMachine(t)
	o := m.options()
	writeFile(t, m.mounts, append(mustRead(t, m.mounts), []byte("91 1 179:4 / /data rw - ext4 /dev/mmcblk0p4 rw\n")...))
	writeFile(t, filepath.Join(m.devDir, "mmcblk0p1"), fatImage())
	o.Modules = []string{"omap_wdt"}

	helper := filepath.Join(m.dir, "sbin", "mdev")
	writeFile(t, helper, []byte("#!/bin/sh\n"))
	o.HotplugHelper = helper
	hotplug := filepath.Join(o.ProcDir, "sys", "kernel", "hotplug")
	writeFile(t, hotplug, []byte(""))

	alias := "usb:v148Fp5370d0101dc00dsc00dp00icFFiscFFipFFin00"
	writeFile(t, filepath.Join(m.sysDir, "bus", "usb", "devices", "1-1:1.0", "modalias"), []byte(alias+"\n"))

	if err := Setup(context.Background(), o); err != nil {
		t.Fatalf("Setup: %v", err)
	}
	if !m.ran("modprobe omap_wdt") {
		t.Errorf("watchdog module was not loaded: %v", m.runs)
	}
	if !m.ran("modprobe -b -q " + alias) {
		t.Errorf("USB modalias was not probed: %v", m.runs)
	}
	if got := string(mustRead(t, hotplug)); got != helper+"\n" {
		t.Errorf("hotplug sysctl = %q, want %q", got, helper+"\n")
	}
}
