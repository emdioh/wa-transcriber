package update

import (
	"runtime"
	"runtime/debug"
)

// DefaultGOARM is the 32-bit ARM level this project releases: the Makefile and
// the image build armv7 only (os/package installs dist/wa-transcriber-linux-armv7).
const DefaultGOARM = "7"

// OwnAssetKey is the manifest asset key of the running binary ("linux-amd64",
// "linux-armv7", …), or an error when this platform has no release asset.
//
// GOARM is not a runtime variable: it is a build setting, recorded in
// debug.ReadBuildInfo when the build passed GOARM=7 (the Makefile does). A
// build that does not record it still gets DefaultGOARM on 32-bit ARM, because
// there is exactly one ARM asset and CheckExecutable compares the ELF header
// before anything is installed. Reading the build tags instead (goarm5/6/7)
// does not work: `go build` never sets them, so the level read as "" on a
// board and every install failed with "unsupported GOARM".
func OwnAssetKey() (string, error) {
	bi, ok := debug.ReadBuildInfo()
	if !ok {
		// A stripped or exotic build records nothing: DefaultGOARM decides on
		// 32-bit ARM, and CheckExecutable still compares the ELF header.
		bi = nil
	}
	return AssetKey(runtime.GOARCH, goarmOf(runtime.GOARCH, bi))
}

// goarmOf picks the GOARM level for a build: the recorded build setting, else
// DefaultGOARM on 32-bit ARM, else "" (AssetKey ignores it off ARM).
func goarmOf(goarch string, bi *debug.BuildInfo) string {
	if goarch != "arm" {
		return ""
	}
	if bi != nil {
		for _, s := range bi.Settings {
			if s.Key == "GOARM" {
				return s.Value
			}
		}
	}
	return DefaultGOARM
}
