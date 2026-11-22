package update

import (
	"debug/elf"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"runtime"
	"strings"
)

// CheckExecutable reports whether path is a regular executable ELF file for
// the machine this process runs on. It answers "would an exec fail here?" so
// Boot can fall back without a crash loop: a wrong-arch or truncated file is
// refused before it is promoted or run.
//
// The arch comes from runtime.GOARCH/GOARM, the same pair AssetKey uses to
// pick the asset; the caller therefore checks the file it just downloaded
// against the binary that is running.
func CheckExecutable(path, assetKey string) error {
	st, err := os.Stat(path)
	if err != nil {
		return fmt.Errorf("update: %s: %w", filepath.Base(path), err)
	}
	if !st.Mode().IsRegular() || st.Size() == 0 {
		return fmt.Errorf("update: %s is not a regular non-empty file", filepath.Base(path))
	}
	if st.Mode().Perm()&0o111 == 0 {
		return fmt.Errorf("update: %s is not executable (mode %v)", filepath.Base(path), st.Mode().Perm())
	}
	f, err := elf.Open(path)
	if err != nil {
		return fmt.Errorf("update: %s is not an ELF binary: %w", filepath.Base(path), err)
	}
	defer f.Close()
	if f.Type != elf.ET_EXEC && f.Type != elf.ET_DYN {
		return fmt.Errorf("update: %s is not an executable (ELF type %v)", filepath.Base(path), f.Type)
	}
	want, err := plainAssetKey(assetKey)
	if err != nil {
		return err
	}
	have, err := ownAssetKey()
	if err != nil {
		return err
	}
	if want != have {
		return fmt.Errorf("update: %s is built for %s, this device runs %s", filepath.Base(path), want, have)
	}
	if err := checkMachine(f, want); err != nil {
		return fmt.Errorf("update: %s: %w", filepath.Base(path), err)
	}
	return nil
}

// plainAssetKey validates a caller-supplied key and strips a platform prefix
// it may carry ("linux-"). Only keys of the running platform are meaningful
// here, so anything else is refused below.
func plainAssetKey(k string) (string, error) {
	k = strings.TrimSpace(k)
	if k == "" {
		return "", errors.New("update: empty asset key")
	}
	if !validKey(k) {
		return "", fmt.Errorf("update: asset key %q", k)
	}
	if !strings.HasPrefix(k, "linux-") {
		return "", fmt.Errorf("update: asset key %q is not a Linux key", k)
	}
	return k, nil
}

// ownAssetKey is AssetKey for the machine this process was built for. GOARM
// is a build-time constant, not an environment variable.
func ownAssetKey() (string, error) { return AssetKey(runtime.GOARCH, goarm) }

// checkMachine compares the ELF header to the expected asset key, being
// stricter than Go's own loader where it is cheap: armv5/armv6/armv7 are
// told apart by their attributes.
func checkMachine(f *elf.File, key string) error {
	switch key {
	case "linux-amd64":
		if f.Machine != elf.EM_X86_64 {
			return fmt.Errorf("ELF machine %v, want x86-64", f.Machine)
		}
	case "linux-arm64":
		if f.Machine != elf.EM_AARCH64 {
			return fmt.Errorf("ELF machine %v, want aarch64", f.Machine)
		}
	case "linux-armv5", "linux-armv6", "linux-armv7":
		if f.Machine != elf.EM_ARM {
			return fmt.Errorf("ELF machine %v, want ARM", f.Machine)
		}
		arch, err := armAttributeArch(f)
		if err != nil {
			return err
		}
		if arch != "" {
			want := strings.TrimPrefix(key, "linux-")
			// v5 and v6 are one kernel ABI for our purposes; v7 is the one
			// that actually matters (ARMv6 binaries run on ARMv7, not the
			// other way round).
			if arch == "v7" && want != "v7" || arch != "v7" && want == "v7" {
				return fmt.Errorf("built for ARM %s, the device is an %s", arch, want)
			}
		}
	default:
		return fmt.Errorf("unsupported asset key %q", key)
	}
	return nil
}

// armAttributeArch reads Tag_CPU_arch from the .ARM.attributes section and
// returns "v5", "v6" or "v7" ("" when the build does not say). The section
// holds a format-version tag and a vendor-specific blob; the first
// subsection's file-scope attributes are what cmd/link writes.
func armAttributeArch(f *elf.File) (string, error) {
	sec := f.Section(".ARM.attributes")
	if sec == nil {
		return "", nil
	}
	data, err := sec.Data()
	if err != nil {
		return "", fmt.Errorf("read .ARM.attributes: %w", err)
	}
	// Skip the format version byte and the "aeabi" vendor name.
	if len(data) < 2+5+1 {
		return "", nil
	}
	data = data[2+5:]
	// One subsection: length (4, includes itself) + name (NUL-terminated) + attributes.
	if len(data) < 4 {
		return "", nil
	}
	name := 0
	for name < len(data) && data[name] != 0 {
		name++
	}
	if name+1 >= len(data) {
		return "", nil
	}
	sub := data[name+1:]
	// Attributes are TLV; only Tag_CPU_arch (6, ULEB128 value) is wanted.
	for i := 0; i < len(sub); {
		tag, n := uleb(sub[i:])
		i += n
		if tag == 0 { // Tag_File: scope, then the list ends
			i += 4
			continue
		}
		if tag == 32 { // Tag_compatibility: ULEB128 + NUL string
			_, n := uleb(sub[i:])
			i += n
			for i < len(sub) && sub[i] != 0 {
				i++
			}
			i++
			continue
		}
		val, n := uleb(sub[i:])
		i += n
		if tag == 6 {
			switch {
			case val <= 8: // Pre-v4 … v5
				return "v5", nil
			case val == 9:
				return "v6", nil
			case val >= 10:
				return "v7", nil
			}
		}
	}
	return "", nil
}

func uleb(b []byte) (uint64, int) {
	var v uint64
	for i := 0; i < len(b) && i < 10; i++ {
		v |= uint64(b[i]&0x7f) << (7 * uint(i))
		if b[i]&0x80 == 0 {
			return v, i + 1
		}
	}
	return v, len(b)
}
