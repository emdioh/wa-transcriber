// Build settings of the running executable: the asset key of a release and
// the compiled-in update key. GOARM is not exported by the runtime, and the
// release build passes GOARM=7 as a setting (Makefile), not the goarm7 build
// tag internal/update compiles its own default from, so the update service
// reads the setting here.
package main

import (
	"crypto/ed25519"
	"log/slog"
	"strings"

	"github.com/emdioh/wa-transcriber/internal/update"
)

// ownAssetKey is the manifest asset key of this executable; "" when the
// platform has no release asset (then the service checks but never installs).
// The GOARM parsing lives in internal/update, next to the code that compares
// an asset key with an ELF header, so the two can never disagree.
func ownAssetKey() string {
	key, err := update.OwnAssetKey()
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
