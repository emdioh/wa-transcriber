// Package update implements binary self-updates for the boards (decision D17,
// ARCHITECTURE.md section 11): the signed release manifest, the version
// ordering, the three-slot state machine that is read at boot, the atomic
// download of a new binary, and the (cached) release check.
//
// The package never execs anything and never touches WhatsApp: Boot returns an
// Action whose Path the caller execs (the factory bootstrap, not
// os.Executable()), and it is written so that no failure of the on-disk state
// can stop the device from starting.
package update

import (
	"bytes"
	"crypto/ed25519"
	"encoding/base64"
	"encoding/json"
	"errors"
	"fmt"
	"sort"
	"strings"
	"time"
)

// Asset is one downloadable file of a release.
type Asset struct {
	File   string `json:"file"`
	SHA256 string `json:"sha256"`
	Size   int64  `json:"size"`
}

// Manifest is the release manifest: manifest.json in the release assets,
// signed detached as manifest.json.ed25519.
type Manifest struct {
	Schema     int       `json:"schema"`
	Version    string    `json:"version"`
	ReleasedAt time.Time `json:"released_at"`
	NotesURL   string    `json:"notes_url"`
	// MinFrom is the oldest version allowed to upgrade straight to this one
	// ("" = any version). It covers session.db schema breaks and other
	// migrations that an intermediate version has to perform first.
	MinFrom string           `json:"min_from"`
	Assets  map[string]Asset `json:"assets"`
}

// ErrNoManifest is returned by Checker.Latest when there is no verified
// manifest yet (no check has succeeded and the cache is empty or stale).
var ErrNoManifest = errors.New("update: no manifest available")

// ParseManifest decodes and validates a manifest. It does not check the
// signature (see VerifyManifest) and does not know the platform: every asset
// has to be well formed, because the manifest is signed as a whole.
func ParseManifest(b []byte) (*Manifest, error) {
	if len(bytes.TrimSpace(b)) == 0 {
		return nil, errors.New("update: empty manifest")
	}
	var m Manifest
	dec := json.NewDecoder(bytes.NewReader(b))
	dec.DisallowUnknownFields()
	if err := dec.Decode(&m); err != nil {
		return nil, fmt.Errorf("update: parse manifest: %w", err)
	}
	if dec.More() {
		return nil, errors.New("update: parse manifest: trailing data")
	}
	if m.Schema != 1 {
		return nil, fmt.Errorf("update: manifest schema %d, want 1", m.Schema)
	}
	if m.Version == "" {
		return nil, errors.New("update: manifest without version")
	}
	if len(m.Assets) == 0 {
		return nil, errors.New("update: manifest without assets")
	}
	keys := make([]string, 0, len(m.Assets))
	for k := range m.Assets {
		keys = append(keys, k)
	}
	sort.Strings(keys)
	for _, k := range keys {
		a := m.Assets[k]
		if !validKey(k) {
			return nil, fmt.Errorf("update: manifest asset key %q", k)
		}
		if a.File == "" {
			return nil, fmt.Errorf("update: asset %q without file", k)
		}
		if !validSHA(a.SHA256) {
			return nil, fmt.Errorf("update: asset %q has bad sha256 %q", k, a.SHA256)
		}
		if a.Size <= 0 {
			return nil, fmt.Errorf("update: asset %q has size %d", k, a.Size)
		}
	}
	return &m, nil
}

// Asset returns the asset for a platform key such as "linux-armv7".
func (m *Manifest) Asset(key string) (Asset, bool) {
	if m == nil {
		return Asset{}, false
	}
	a, ok := m.Assets[key]
	return a, ok
}

// AllowsUpgradeFrom reports whether a device on current may install this
// manifest. An empty MinFrom means any version may.
func (m *Manifest) AllowsUpgradeFrom(current string) bool {
	if m == nil {
		return false
	}
	if m.MinFrom == "" {
		return true
	}
	return Compare(current, m.MinFrom) >= 0
}

// isHex reports whether c is a lowercase hex digit.
func isHex(c byte) bool {
	return c >= '0' && c <= '9' || c >= 'a' && c <= 'f'
}

func validSHA(s string) bool {
	if len(s) != 64 {
		return false
	}
	for i := 0; i < len(s); i++ {
		if !isHex(s[i]) {
			return false
		}
	}
	return true
}

// validKey accepts platform keys made of [a-z0-9_-]; it also keeps an asset
// key from being used as a path when it ends up in a file name.
func validKey(k string) bool {
	if k == "" {
		return false
	}
	for i := 0; i < len(k); i++ {
		c := k[i]
		if c >= 'a' && c <= 'z' || c >= '0' && c <= '9' || c == '-' || c == '_' {
			continue
		}
		return false
	}
	return true
}

// SafeVersion reports whether v may be used in a file name: it ends up in
// wa-transcriber-<version>, so it is limited to the characters git describe
// produces (letters, digits, '.', '-', '_', '+') and must not start with a
// dot or a dash.
func SafeVersion(v string) bool {
	if v == "" || len(v) > 128 {
		return false
	}
	if v[0] == '.' || v[0] == '-' {
		return false
	}
	for i := 0; i < len(v); i++ {
		c := v[i]
		if c >= '0' && c <= '9' || c >= 'a' && c <= 'z' || c >= 'A' && c <= 'Z' ||
			c == '.' || c == '-' || c == '_' || c == '+' {
			continue
		}
		return false
	}
	return true
}

// AssetKey maps a Go platform to the manifest asset key:
// linux-amd64, linux-arm64, linux-armv5, linux-armv6, linux-armv7.
// GOARM may carry a float ABI suffix (which the manifest does not need).
func AssetKey(goarch, goarm string) (string, error) {
	switch goarch {
	case "amd64", "arm64":
		return "linux-" + goarch, nil
	case "arm":
		v := goarm
		if i := strings.IndexByte(v, ','); i >= 0 {
			v = v[:i]
		}
		switch v {
		case "5", "6", "7":
			return "linux-armv" + v, nil
		default:
			return "", fmt.Errorf("update: unsupported GOARM %q", goarm)
		}
	default:
		return "", fmt.Errorf("update: unsupported architecture %q", goarch)
	}
}

// VerifyManifest checks the detached ed25519 signature over the raw manifest
// bytes (the file as served, not a re-encoding). A nil public key means the
// build has no key compiled in, so updates are off and nothing verifies.
func VerifyManifest(pub ed25519.PublicKey, manifest, sig []byte) error {
	if len(pub) != ed25519.PublicKeySize {
		return errors.New("update: no update public key")
	}
	if len(sig) != ed25519.SignatureSize {
		return fmt.Errorf("update: signature is %d bytes, want %d", len(sig), ed25519.SignatureSize)
	}
	if !ed25519.Verify(pub, manifest, sig) {
		return errors.New("update: bad manifest signature")
	}
	return nil
}

// PublicKey decodes a base64 (standard or raw, padded or not) ed25519 public
// key such as the one in release/update-pubkey.txt. An empty string returns
// (nil, nil): no key means no updates.
func PublicKey(b64 string) (ed25519.PublicKey, error) {
	s := strings.TrimSpace(b64)
	if s == "" {
		return nil, nil
	}
	var raw []byte
	var err error
	for _, enc := range []*base64.Encoding{
		base64.StdEncoding, base64.RawStdEncoding, base64.URLEncoding, base64.RawURLEncoding,
	} {
		raw, err = enc.DecodeString(s)
		if err == nil {
			break
		}
	}
	if err != nil {
		return nil, fmt.Errorf("update: decode public key: %w", err)
	}
	if len(raw) != ed25519.PublicKeySize {
		return nil, fmt.Errorf("update: public key is %d bytes, want %d", len(raw), ed25519.PublicKeySize)
	}
	return ed25519.PublicKey(raw), nil
}
