package main

import (
	"flag"
	"slices"
	"strings"
	"testing"
)

// noEnv is a machine with no WAT_* variable set, which is a board.
func noEnv(string) (string, bool) { return "", false }

// TestSetInitEnv: the kernel gives init no PATH, and every helper is run by
// bare name.
func TestSetInitEnv(t *testing.T) {
	env := map[string]string{"HOME": "/"}
	setInitEnv(func(k string) (string, bool) { v, ok := env[k]; return v, ok },
		func(k, v string) error { env[k] = v; return nil })
	want := "/sbin:/usr/sbin:/bin:/usr/bin"
	if env["PATH"] != want {
		t.Fatalf("PATH = %q, want %q", env["PATH"], want)
	}
	// An environment that has one (a service manager, Docker) is left alone.
	env = map[string]string{"PATH": "/usr/local/bin"}
	setInitEnv(func(k string) (string, bool) { v, ok := env[k]; return v, ok },
		func(k, v string) error { env[k] = v; return nil })
	if env["PATH"] != "/usr/local/bin" {
		t.Fatalf("PATH = %q, want the caller's", env["PATH"])
	}
}

func TestWatArgs(t *testing.T) {
	got := watArgs("root=/dev/mmcblk0p2 ro wat.network=none wat.listen=:8080 wat.broken wat.=x")
	if len(got) != 2 || got["network"] != "none" || got["listen"] != ":8080" {
		t.Fatalf("watArgs = %v", got)
	}
}

func TestApplyInitDefaults(t *testing.T) {
	tests := []struct {
		name     string
		argv     []string
		cmdline  string
		want     map[string]string
		notGiven []string
	}{
		{
			name: "board defaults",
			argv: nil,
			want: map[string]string{
				"data": "/data", "listen": ":80", "network": "managed",
				"watchdog": "/dev/watchdog", "update-dir": "/data/bin",
				"image-update": "/data", "bootstrap": "/usr/bin/wa-transcriber",
			},
		},
		{
			name:    "kernel command line wins over the default",
			argv:    []string{"-listen", ":8080"},
			cmdline: "init=/usr/bin/wa-transcriber wat.network=none",
			want: map[string]string{
				"listen": ":8080", "network": "none", "data": "/data",
			},
		},
		{
			name:    "a flag on the command line is never touched",
			argv:    []string{"-network", "none"},
			cmdline: "wat.network=managed",
			want:    map[string]string{"network": "none"},
		},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			fs := flag.NewFlagSet(tc.name, flag.ContinueOnError)
			data := fs.String("data", "", "")
			listen := fs.String("listen", "", "")
			network := fs.String("network", "", "")
			watchdog := fs.String("watchdog", "", "")
			updateDir := fs.String("update-dir", "", "")
			imageUpdate := fs.String("image-update", "", "")
			bootstrap := fs.String("bootstrap", "", "")
			// A flag the package knows nothing about stays empty.
			logLevel := fs.String("log-level", "", "")
			if err := fs.Parse(tc.argv); err != nil {
				t.Fatal(err)
			}
			applied, err := applyInitDefaults(fs, tc.cmdline, noEnv)
			if err != nil {
				t.Fatalf("applyInitDefaults: %v", err)
			}
			got := map[string]string{
				"data": *data, "listen": *listen, "network": *network,
				"watchdog": *watchdog, "update-dir": *updateDir,
				"image-update": *imageUpdate, "bootstrap": *bootstrap,
			}
			for name, want := range tc.want {
				if got[name] != want {
					t.Errorf("%s = %q, want %q", name, got[name], want)
				}
			}
			if *logLevel != "" {
				t.Errorf("log-level was set to %q by the init defaults", *logLevel)
			}
			given := map[string]bool{}
			for _, a := range tc.argv {
				if name, ok := strings.CutPrefix(a, "-"); ok {
					given[name] = true
				}
			}
			for name, value := range got {
				if value == "" || given[name] {
					continue
				}
				want := name + "=" + value
				if !slices.Contains(applied, want) {
					t.Errorf("applied = %v, want it to contain %q", applied, want)
				}
			}
			for _, a := range applied {
				if !strings.Contains(a, "=") {
					t.Errorf("applied entry %q has no value", a)
				}
			}
		})
	}
}

// TestApplyInitDefaultsLeavesEnvironmentAlone: the container image sets
// WAT_LISTEN, and a board started with -init must not move the web UI to :80
// because of a default.
func TestApplyInitDefaultsLeavesEnvironmentAlone(t *testing.T) {
	fs := flag.NewFlagSet("t", flag.ContinueOnError)
	listen := fs.String("listen", ":8080", "") // as if from WAT_LISTEN
	if err := fs.Parse(nil); err != nil {
		t.Fatal(err)
	}
	lookup := func(name string) (string, bool) { return ":8080", name == "WAT_LISTEN" }
	if _, err := applyInitDefaults(fs, "init=/usr/bin/wa-transcriber", lookup); err != nil {
		t.Fatal(err)
	}
	if *listen != ":8080" {
		t.Fatalf("listen = %q, want the environment's :8080", *listen)
	}
}

func TestApplyInitDefaultsRejectsABadValue(t *testing.T) {
	fs := flag.NewFlagSet("t", flag.ContinueOnError)
	watchdog := fs.String("watchdog", "", "")
	if _, err := applyInitDefaults(fs, "wat.watchdog=", noEnv); err != nil {
		t.Fatalf("an empty value is a value: %v", err)
	}
	if *watchdog != "" {
		t.Fatalf("watchdog = %q", *watchdog)
	}
}
