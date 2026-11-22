package update

import (
	"crypto/ed25519"
	"crypto/rand"
	"encoding/base64"
	"strings"
	"testing"
)

func TestCompare(t *testing.T) {
	tests := []struct {
		a, b string
		want int // sign only: -1, 0, 1
	}{
		// Plain tags, missing parts are zero.
		{"v1.2", "v1.2.0", 0},
		{"1.2.3", "v1.2.3", 0},
		{"v1.2.3", "v1.2.4", -1},
		{"v2.0.0", "v1.99.99", 1},
		{"v0.1.1", "v0.1.0", 1},
		// Real `git describe --tags` shapes.
		{"v1.2.3", "v1.2.3-4-gabc", 1},
		{"v1.2.3-4-gabc", "v1.2.3-2-gabc", 1},
		{"v1.2.3-2-gabc", "v1.2.3-2-gabc", 0},
		{"v1.2.4-1-gabc", "v1.2.3", 1},
		{"v1.2.3-10-gabc", "v1.2.3-9-gabc", 1},
		{"v0.1.0-3-gf2a1bd9", "v0.1.0", -1},
		// Dirty trees and build metadata.
		{"v1.2.3-dirty", "v1.2.3", 0},
		{"v1.2.3-4-gabc-dirty", "v1.2.3-4-gabc", 0},
		{"v1.2.3+g1234", "v1.2.3", 0},
		// Development builds outrank every release.
		{"dev", "v9.9.9", 1},
		{"", "v0.0.1", 1},
		{"v9.9.9", "dev", -1},
		// Odd shapes fall back to the string order but stay antisymmetric.
		{"main", "v1.0.0", -1},
		{"xyz", "v1.0.0", -1},
		{"xyz", "xyz", 0},
	}
	for _, tc := range tests {
		got := Compare(tc.a, tc.b)
		if sign(got) != tc.want {
			t.Errorf("Compare(%q, %q) = %d, want sign %d", tc.a, tc.b, got, tc.want)
		}
		if rev := sign(Compare(tc.b, tc.a)); rev != -tc.want {
			t.Errorf("Compare(%q, %q) = %d, not the reverse of Compare(%q, %q)", tc.b, tc.a, rev, tc.a, tc.b)
		}
	}
}

func sign(n int) int {
	switch {
	case n < 0:
		return -1
	case n > 0:
		return 1
	}
	return 0
}

func TestIsUpgrade(t *testing.T) {
	tests := []struct {
		current, candidate string
		want               bool
	}{
		{"v0.1.1", "v0.2.0", true},
		{"v0.1.1", "v0.1.1", false}, // same version is not an upgrade
		{"v0.2.0", "v0.1.1", false}, // never a silent downgrade
		{"v0.1.1", "", false},
		{"v0.1.1", "garbage", false},
		{"v0.1.1-3-gabc", "v0.2.0", true},
		{"v0.1.1", "v0.1.1-1-gabc", false}, // a commit after the tag is not newer for an install
		{"dev", "v1.0.0", false},
		{"v1.0.0", "dev", true},
	}
	for _, tc := range tests {
		if got := IsUpgrade(tc.current, tc.candidate); got != tc.want {
			t.Errorf("IsUpgrade(%q, %q) = %v, want %v", tc.current, tc.candidate, got, tc.want)
		}
	}
}

const goodSHA = "9f86d081884c7d659a2feaa0c55ad015a3bf4f1b2b0b822cd15d6c15b0f00a08"

func goodManifestJSON(extra string) string {
	return `{"schema":1,"version":"v0.2.0","released_at":"2026-11-20T10:00:00Z",` +
		`"notes_url":"https://example.invalid/notes","min_from":"v0.1.0",` +
		`"assets":{"linux-armv7":{"file":"wa-transcriber-v0.2.0-linux-armv7","sha256":"` + goodSHA +
		`","size":12345}` + extra + `}}`
}

func TestParseManifest(t *testing.T) {
	tests := []struct {
		name string
		in   string
		ok   bool
	}{
		{"good", goodManifestJSON(""), true},
		{"good two assets", goodManifestJSON(`, "linux-amd64": {"file":"x","sha256":"` + goodSHA + `","size":1}`), true},
		{"empty", "", false},
		{"not json", "{", false},
		{"bad schema 2", strings.Replace(goodManifestJSON(""), `"schema":1`, `"schema":2`, 1), false},
		{"schema missing", strings.Replace(goodManifestJSON(""), `"schema":1,`, ``, 1), false},
		{"no version", strings.Replace(goodManifestJSON(""), `"version":"v0.2.0"`, `"version":""`, 1), false},
		{"no assets", strings.Replace(goodManifestJSON(""), `"assets":{`, `"assets":{},"x":{`, 1), false},
		{"bad sha short", strings.Replace(goodManifestJSON(""), goodSHA, goSHAshort, 1), false},
		{"bad sha uppercase", strings.Replace(goodManifestJSON(""), goodSHA, strings.ToUpper(goodSHA), 1), false},
		{"bad sha non-hex", strings.Replace(goodManifestJSON(""), goodSHA, strings.Repeat("z", 64), 1), false},
		{"zero size", strings.Replace(goodManifestJSON(""), `"size":12345`, `"size":0`, 1), false},
		{"negative size", strings.Replace(goodManifestJSON(""), `"size":12345`, `"size":-5`, 1), false},
		{"empty file", strings.Replace(goodManifestJSON(""), `"file":"wa-transcriber-v0.2.0-linux-armv7"`, `"file":""`, 1), false},
		{"unknown field", strings.Replace(goodManifestJSON(""), `"schema":1`, `"schema":1,"bogus":true`, 1), false},
		{"bad asset key", strings.Replace(goodManifestJSON(""), `"linux-armv7"`, `"linux/armv7"`, 1), false},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			m, err := ParseManifest([]byte(tc.in))
			if tc.ok {
				if err != nil {
					t.Fatalf("ParseManifest: unexpected error %v", err)
				}
				if m.Version != "v0.2.0" {
					t.Fatalf("version = %q", m.Version)
				}
				a, ok := m.Asset("linux-armv7")
				if !ok || a.Size != 12345 || a.SHA256 != goodSHA {
					t.Fatalf("asset = %+v, %v", a, ok)
				}
				if _, ok := m.Asset("linux-arm64"); ok {
					t.Fatal("unexpected arm64 asset")
				}
				if !m.AllowsUpgradeFrom("v0.1.0") || !m.AllowsUpgradeFrom("v0.1.5") {
					t.Fatal("AllowsUpgradeFrom refused a newer current version")
				}
				if m.AllowsUpgradeFrom("v0.0.9") {
					t.Fatal("AllowsUpgradeFrom accepted a version below min_from")
				}
				return
			}
			if err == nil {
				t.Fatalf("ParseManifest accepted %s", tc.name)
			}
		})
	}
}

const goSHAshort = "9f86d081884c7d659a2feaa0c55ad015a3bf4f1b2b0b822cd15d6c15b0f00a0"

func TestManifestAllowsUpgradeFromEmptyMin(t *testing.T) {
	m, err := ParseManifest([]byte(strings.Replace(goodManifestJSON(""), `"min_from":"v0.1.0"`, `"min_from":""`, 1)))
	if err != nil {
		t.Fatal(err)
	}
	if !m.AllowsUpgradeFrom("v0.0.1") || !m.AllowsUpgradeFrom("dev") {
		t.Fatal("empty min_from must allow every current version")
	}
}

func TestPublicKey(t *testing.T) {
	pub, _, err := ed25519.GenerateKey(rand.Reader)
	if err != nil {
		t.Fatal(err)
	}
	enc := base64.StdEncoding.EncodeToString(pub)
	// mode: 0 = empty (nil, nil), 1 = valid key, 2 = error
	tests := []struct {
		name string
		in   string
		mode int
	}{
		{"empty", "", 0},
		{"whitespace", "  \n\t ", 0},
		{"padded", enc, 1},
		{"with newline", enc + "\n", 1},
		{"raw", base64.RawStdEncoding.EncodeToString(pub), 1},
		{"not base64", "!!!!", 2},
		{"too short", base64.StdEncoding.EncodeToString(pub[:16]), 2},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			k, err := PublicKey(tc.in)
			switch tc.mode {
			case 0:
				if k != nil || err != nil {
					t.Fatalf("PublicKey(%q) = %x, %v; want nil, nil", tc.in, k, err)
				}
			case 1:
				if err != nil || string(k) != string(pub) {
					t.Fatalf("PublicKey(%q) = %x, %v", tc.in, k, err)
				}
			default:
				if err == nil {
					t.Fatalf("PublicKey(%q) = %x, want an error", tc.in, k)
				}
			}
		})
	}
}

func TestVerifyManifest(t *testing.T) {
	pub, priv, err := ed25519.GenerateKey(rand.Reader)
	if err != nil {
		t.Fatal(err)
	}
	otherPub, _, err := ed25519.GenerateKey(rand.Reader)
	if err != nil {
		t.Fatal(err)
	}
	body := []byte(goodManifestJSON(""))
	sig := ed25519.Sign(priv, body)
	tampered := []byte(strings.Replace(goodManifestJSON(""), "v0.2.0", "v0.3.0", 1))

	tests := []struct {
		name string
		pub  ed25519.PublicKey
		body []byte
		sig  []byte
		ok   bool
	}{
		{"signed", pub, body, sig, true},
		{"tampered", pub, tampered, sig, false},
		{"wrong key", otherPub, body, sig, false},
		{"nil pub", nil, body, sig, false},
		{"empty pub", ed25519.PublicKey{}, body, sig, false},
		{"truncated sig", pub, body, sig[:10], false},
		{"no sig", pub, body, nil, false},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			err := VerifyManifest(tc.pub, tc.body, tc.sig)
			if tc.ok && err != nil {
				t.Fatalf("VerifyManifest: %v", err)
			}
			if !tc.ok && err == nil {
				t.Fatal("VerifyManifest accepted an invalid manifest")
			}
		})
	}
}

func TestAssetKey(t *testing.T) {
	tests := []struct {
		arch, goarm, want string
		ok                bool
	}{
		{"amd64", "", "linux-amd64", true},
		{"arm64", "", "linux-arm64", true},
		{"arm", "5", "linux-armv5", true},
		{"arm", "6", "linux-armv6", true},
		{"arm", "7", "linux-armv7", true},
		{"arm", "7,hardfloat", "linux-armv7", true},
		{"arm", "", "", false},
		{"arm", "8", "", false},
		{"386", "", "", false},
		{"mips", "", "", false},
	}
	for _, tc := range tests {
		got, err := AssetKey(tc.arch, tc.goarm)
		if tc.ok != (err == nil) {
			t.Errorf("AssetKey(%q, %q) err = %v", tc.arch, tc.goarm, err)
			continue
		}
		if got != tc.want {
			t.Errorf("AssetKey(%q, %q) = %q, want %q", tc.arch, tc.goarm, got, tc.want)
		}
	}
}

func TestSafeVersion(t *testing.T) {
	good := []string{"v1.2.3", "v1.2.3-4-gabc", "v1.2.3-4-gabc-dirty", "dev", "v0.1.0+meta"}
	bad := []string{"", ".", "..", "-rf", "../etc", "a/b", "a b", "a\x00b", strings.Repeat("v", 129)}
	for _, v := range good {
		if !SafeVersion(v) {
			t.Errorf("SafeVersion(%q) = false", v)
		}
	}
	for _, v := range bad {
		if SafeVersion(v) {
			t.Errorf("SafeVersion(%q) = true", v)
		}
	}
}
