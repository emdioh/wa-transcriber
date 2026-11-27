package netmgr

import (
	"errors"
	"fmt"
	"os"
	"strings"
)

// WatTxtPath is the installer-edited file on the FAT boot partition. It is
// mounted read-only at /boot, so the values only change between boots.
const WatTxtPath = "/boot/wat.txt"

// watValue returns the value of key in a wat.txt body. The file is written on
// other machines, so Windows line endings, '#' comment lines and spaces
// around the '=' are tolerated; the value itself is only trimmed, because a
// passphrase may contain '#' or '='.
func watValue(data []byte, key string) string {
	for _, line := range strings.Split(string(data), "\n") {
		line = strings.TrimSpace(line)
		if line == "" || strings.HasPrefix(line, "#") {
			continue
		}
		k, v, ok := strings.Cut(line, "=")
		if !ok || strings.TrimSpace(k) != key {
			continue
		}
		return strings.TrimSpace(v)
	}
	return ""
}

// watLastValue returns the last value of key in a wat.txt body (so a line
// appended at the bottom overrides an earlier template default).
func watLastValue(data []byte, key string) string {
	got := ""
	for _, line := range strings.Split(string(data), "\n") {
		line = strings.TrimSpace(line)
		if line == "" || strings.HasPrefix(line, "#") {
			continue
		}
		k, v, ok := strings.Cut(line, "=")
		if !ok || strings.TrimSpace(k) != key {
			continue
		}
		got = strings.TrimSpace(v)
	}
	return got
}

// DefaultHostname is the mDNS label used when wat.txt does not set hostname=.
const DefaultHostname = "wa"

// Hostname reads hostname= from wat.txt and returns a normalised RFC 1123
// single-label mDNS name (default "wa"). An optional ".local" suffix and
// inline "# comment" are tolerated. When the file is absent or empty it
// returns DefaultHostname with a nil error; when the value is invalid it
// returns DefaultHostname along with an error describing why it was ignored.
func Hostname(path string) (string, error) {
	data, err := os.ReadFile(path)
	if err != nil {
		if errors.Is(err, os.ErrNotExist) {
			return DefaultHostname, nil
		}
		return DefaultHostname, err
	}
	raw := watLastValue(data, "hostname")
	if i := strings.IndexByte(raw, '#'); i >= 0 {
		raw = strings.TrimSpace(raw[:i])
	}
	if raw == "" {
		return DefaultHostname, nil
	}
	name := strings.ToLower(raw)
	name = strings.TrimSuffix(name, ".")
	name = strings.TrimSuffix(name, ".local")
	if !validHostnameLabel(name) {
		return DefaultHostname, fmt.Errorf("hostname %q: must be 1-63 ASCII letters, digits or hyphens", raw)
	}
	return name, nil
}

func validHostnameLabel(s string) bool {
	if len(s) == 0 || len(s) > 63 || s[0] == '-' || s[len(s)-1] == '-' {
		return false
	}
	for i := 0; i < len(s); i++ {
		c := s[i]
		if (c >= 'a' && c <= 'z') || (c >= '0' && c <= '9') || c == '-' {
			continue
		}
		return false
	}
	return true
}

// ProvisionWifi makes the network named in wat.txt a saved network, so a card
// can be prepared with the WiFi details before the first boot and the device
// joins without anyone touching the web UI. It returns the SSID when the
// store changed.
//
// A passphrase that cannot work (WPA needs 8 to 63 characters) is an error and
// not a silent skip: the device would otherwise sit in the setup AP with no
// hint about the typo on the card. The passphrase is never logged and never
// stored: WifiStore keeps the derived PSK only.
func ProvisionWifi(path string, store *WifiStore) (string, error) {
	if store == nil {
		return "", nil
	}
	data, err := os.ReadFile(path)
	if err != nil {
		if errors.Is(err, os.ErrNotExist) {
			return "", nil
		}
		return "", err
	}
	ssid := watValue(data, "wifi_ssid")
	if ssid == "" {
		return "", nil
	}
	pass := watValue(data, "wifi_passphrase")
	if len(pass) < 8 || len(pass) > 63 {
		return "", fmt.Errorf("wifi_passphrase for %q: WPA needs 8 to 63 characters", ssid)
	}
	changed, err := store.Ensure(ssid, pass)
	if err != nil || !changed {
		return "", err
	}
	return ssid, nil
}
