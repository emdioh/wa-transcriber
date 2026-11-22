package update

import (
	"strconv"
	"strings"
)

// Compare orders two version strings. Negative means a < b, zero equal,
// positive a > b. It is deliberately small and only knows the shapes this
// project produces, but those it gets right:
//
//	"v1.2"            == "v1.2.0"           (missing parts are zero)
//	"v1.2.3"          >  "v1.2.3-4-gabc"    (a clean tag beats a commit after it)
//	"v1.2.3-4-gabc"   >  "v1.2.3-2-gabc"    (more commits since the tag is newer)
//	"v1.2.4-1-gabc"   >  "v1.2.3"           (the numeric part decides first)
//	"v1.2.3+meta"     == "v1.2.3"           (build metadata is ignored)
//	"dev" and ""      >  everything         (a dev build never downgrades itself)
//
// The shape is `git describe --tags [--long] [--dirty]`: "vMAJOR.MINOR.PATCH",
// optionally followed by "-N-gHASH" and "-dirty", or a bare tag/branch name.
// Rule: first the numeric MAJOR.MINOR.PATCH (missing = 0, leading zeros are
// fine); more commits after the tag (N) makes a version newer within the same
// numeric version; a plain tag is newer than any commit counted on top of it.
// If neither side has a numeric core, the strings are compared as-is. "dev"
// and "" rank above every version so that a development binary is never
// replaced by an older release.
func Compare(a, b string) int {
	adev, bdev := isDev(a), isDev(b)
	switch {
	case adev && bdev:
		return strings.Compare(a, b)
	case adev:
		return 1
	case bdev:
		return -1
	}
	an, ac, aok := parseVersion(a)
	bn, bc, bok := parseVersion(b)
	if !aok || !bok {
		// Unknown shapes: fall back to a plain string order. This is wrong
		// for exotic versions but stable, and never claims equality of
		// different strings.
		if aok {
			return 1
		}
		if bok {
			return -1
		}
		return strings.Compare(a, b)
	}
	for i := 0; i < 3; i++ {
		if an[i] != bn[i] {
			if an[i] < bn[i] {
				return -1
			}
			return 1
		}
	}
	switch {
	case ac < 0 && bc < 0:
		return 0
	case ac < 0: // a is the tag itself: newer
		return 1
	case bc < 0:
		return -1
	case ac == bc:
		return 0
	case ac < bc:
		return -1
	default:
		return 1
	}
}

// IsUpgrade reports whether moving from current to candidate is an upgrade.
//
// Equal versions are not upgrades (re-installing the same version is a
// no-op for the UI, a reflash or a manual install can still force it). A
// candidate that cannot be parsed is never an upgrade. "dev" is not special
// here beyond Compare: a running dev build never upgrades to a release, and a
// running release always upgrades to a "dev" manifest (which only a test
// server serves, and the signature still has to verify).
func IsUpgrade(current, candidate string) bool {
	if strings.TrimSpace(candidate) == "" {
		return false
	}
	if _, _, ok := parseVersion(candidate); !ok && !isDev(candidate) {
		return false
	}
	return Compare(candidate, current) > 0
}

// isDev reports the development versions that outrank every release.
func isDev(v string) bool {
	switch strings.TrimSpace(v) {
	case "dev", "":
		return true
	}
	return false
}

// parseVersion splits a describe-style version into its numeric core and the
// commit count since the tag (-1 when the string is the tag itself). ok is
// false when there is no numeric core at all.
func parseVersion(v string) (num [3]int, count int, ok bool) {
	s := strings.TrimSpace(v)
	s = strings.TrimPrefix(s, "v")
	s = strings.TrimPrefix(s, "V")
	if i := strings.IndexByte(s, '+'); i >= 0 { // build metadata: ignored
		s = s[:i]
	}
	count = -1
	// The git describe tail is always the last two dash-separated fields
	// (-N-gHASH, -N-gHASH-dirty). Strip "-dirty" first, then look back.
	s = strings.TrimSuffix(s, "-dirty")
	if i := strings.LastIndexByte(s, '-'); i >= 0 {
		if j := strings.LastIndexByte(s[:i], '-'); j >= 0 {
			if n, err := strconv.Atoi(s[j+1 : i]); err == nil && n >= 0 &&
				strings.HasPrefix(s[i+1:], "g") && isHexString(s[i+2:]) && s[i+2:] != "" {
				count = n
				s = s[:j]
			}
		}
	}
	parts := strings.Split(s, ".")
	if len(parts) > 3 || len(parts) == 0 {
		return num, count, false
	}
	for i, p := range parts {
		if p == "" || len(p) > 18 {
			return num, count, false
		}
		n, err := strconv.Atoi(p)
		if err != nil || n < 0 {
			return num, count, false
		}
		num[i] = n
	}
	return num, count, true
}

func isHexString(s string) bool {
	if s == "" {
		return false
	}
	for i := 0; i < len(s); i++ {
		if !isHex(s[i]) && !(s[i] >= 'A' && s[i] <= 'F') {
			return false
		}
	}
	return true
}
