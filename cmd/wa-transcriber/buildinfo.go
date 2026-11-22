// Build settings of the running executable: the asset key of a release and
// the compiled-in update key. GOARM is not exported by the runtime, and the
// release build passes GOARM=7 as a setting (Makefile), not the goarm7 build
// tag internal/update compiles its own default from, so the update service
// reads the setting here.
package main

import (
	"crypto/ed25519"
	"log/slog"
	"runtime"
	"runtime/debug"
	"strings"

	"github.com/emdioh/wa-transcriber/internal/update"
)

// defaultGOARM is the 32-bit ARM level this project releases (Makefile:
// GOARM=7; os/package installs the armv7 binary). It is assumed when the build
// info does not say, because there is only one ARM asset to offer and
// CheckExecutable still compares the ELF header before anything is installed.
const defaultGOARM = "7"

// assetKeyOf returns the manifest asset key of a binary with the given
// GOARCH/GOARM ("linux-amd64", "linux-armv7", …). It is a variable so the
// tests can pin the platform.
var assetKeyOf = update.AssetKey

// goarmOf returns the GOARM level of the running build: the GOARM build
// setting when recorded, defaultGOARM for 32-bit ARM, "" elsewhere.
func goarmOf(bi *debug.BuildInfo) string {
	if runtime.GOARCH != "arm" {
		return ""
	}
	if bi != nil {
		for _, s := range bi.Settings {
			if s.Key == "GOARM" {
				return s.Value
			}
		}
	}
	return defaultGOARM
}

// ownAssetKey is the asset key of this executable; "" when the platform has
// no release asset (then the service checks but never installs).
func ownAssetKey() string {
	bi, _ := debug.ReadBuildInfo()
	key, err := assetKeyOf(runtime.GOARCH, goarmOf(bi))
	if err != nil {
		return ""
	}
	return key
}

// resolveUpdateKey decodes -X main.updatePubKey. An empty value means the
// feature is compiled out (Docker, dev builds) and gives a nil key without
// noise; a value that does not decode is worth a warning.
func resolveUpdateKey(log *slog.Logger, b64 string) ed25519.PublicKey {
	key, err := update.PublicKey(strings.TrimSpace(b64))
	if err != nil {
		log.Warn("update: the update public key does not decode, self-updates are off", "err", err)
		return nil
	}
	return key
}
