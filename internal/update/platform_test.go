package update

import (
	"runtime"
	"runtime/debug"
	"testing"
)

// buildInfo builds a BuildInfo holding only the settings a release build
// records.
func buildInfo(pairs ...string) *debug.BuildInfo {
	bi := &debug.BuildInfo{}
	for i := 0; i+1 < len(pairs); i += 2 {
		bi.Settings = append(bi.Settings, debug.BuildSetting{Key: pairs[i], Value: pairs[i+1]})
	}
	return bi
}

// TestGoarmOf pins the level a board runs at. The bug this guards against read
// the goarm5/6/7 build tags, which `go build` never sets: on the BeagleBone
// Black the level then came out empty and every install failed with
// "unsupported GOARM".
func TestGoarmOf(t *testing.T) {
	cases := []struct {
		name   string
		goarch string
		bi     *debug.BuildInfo
		want   string
	}{
		{"arm records GOARM=7", "arm", buildInfo("GOARM", "7"), "7"},
		{"arm records GOARM=6", "arm", buildInfo("GOARM", "6"), "6"},
		{"arm without the setting", "arm", buildInfo("vcs.revision", "abc"), DefaultGOARM},
		{"arm without build info", "arm", nil, DefaultGOARM},
		{"amd64 ignores GOARM", "amd64", buildInfo("GOARM", "7"), ""},
		{"arm64 ignores GOARM", "arm64", buildInfo("GOARM", "7"), ""},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			if got := goarmOf(c.goarch, c.bi); got != c.want {
				t.Fatalf("goarmOf(%q) = %q, want %q", c.goarch, got, c.want)
			}
		})
	}
}

// TestOwnAssetKey checks the key of the platform the test runs on, which is
// also what CheckExecutable compares a download against.
func TestOwnAssetKey(t *testing.T) {
	got, err := OwnAssetKey()
	if err != nil {
		t.Fatalf("OwnAssetKey: %v", err)
	}
	want := "linux-" + runtime.GOARCH
	if runtime.GOARCH == "arm" {
		want = "linux-armv7"
	}
	if got != want {
		t.Fatalf("OwnAssetKey = %q, want %q", got, want)
	}
}
