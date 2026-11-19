package netmgr

import (
	"crypto/pbkdf2"
	"crypto/sha1"
	"encoding/hex"
	"encoding/json"
	"errors"
	"os"
	"path/filepath"
	"sort"
	"sync"
	"time"
)

// SavedNetwork is one WiFi network remembered by the device. The passphrase
// is never stored: PSK holds the 64-hex WPA PSK derived from it.
type SavedNetwork struct {
	SSID     string    `json:"ssid"`
	PSK      string    `json:"psk"`
	LastOK   time.Time `json:"last_ok,omitempty"`
	Priority int       `json:"priority"`
}

type wifiFile struct {
	Networks []SavedNetwork `json:"networks"`
}

// WifiStore is the concurrency-safe handle on /data/wifi.json. Writes are
// atomic (temp file + fsync + rename), like config.json.
type WifiStore struct {
	path string
	mu   sync.Mutex
	cur  wifiFile
}

// OpenWifiStore loads path, or starts empty when it does not exist.
func OpenWifiStore(path string) (*WifiStore, error) {
	s := &WifiStore{path: path}
	data, err := os.ReadFile(path)
	switch {
	case errors.Is(err, os.ErrNotExist):
		return s, nil
	case err != nil:
		return nil, err
	}
	if err := json.Unmarshal(data, &s.cur); err != nil {
		return nil, err
	}
	return s, nil
}

// List returns a copy of the saved networks, highest priority first.
func (s *WifiStore) List() []SavedNetwork {
	s.mu.Lock()
	defer s.mu.Unlock()
	out := make([]SavedNetwork, len(s.cur.Networks))
	copy(out, s.cur.Networks)
	sort.SliceStable(out, func(i, j int) bool { return out[i].Priority > out[j].Priority })
	return out
}

// Save adds or replaces ssid with the given passphrase. New networks get the
// highest priority so they are tried first.
func (s *WifiStore) Save(ssid, passphrase string) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	next := s.cur
	key, err := pbkdf2.Key(sha1.New, passphrase, []byte(ssid), 4096, 32)
	if err != nil {
		return err
	}
	psk := hex.EncodeToString(key)
	prio := 1
	for _, n := range next.Networks {
		if n.Priority >= prio {
			prio = n.Priority + 1
		}
	}
	replaced := false
	for i := range next.Networks {
		if next.Networks[i].SSID == ssid {
			next.Networks[i].PSK = psk
			next.Networks[i].Priority = prio
			next.Networks[i].LastOK = time.Time{}
			replaced = true
			break
		}
	}
	if !replaced {
		next.Networks = append(next.Networks, SavedNetwork{SSID: ssid, PSK: psk, Priority: prio})
	}
	if err := writeWifi(s.path, next); err != nil {
		return err
	}
	s.cur = next
	return nil
}

// MarkOK records a successful connection and moves ssid to the front
// (highest priority).
func (s *WifiStore) MarkOK(ssid string, at time.Time) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	next := s.cur
	found := false
	for i := range next.Networks {
		if next.Networks[i].SSID == ssid {
			next.Networks[i].LastOK = at
			found = true
		}
	}
	if !found {
		return nil
	}
	prio := 1
	for _, n := range next.Networks {
		if n.SSID != ssid && n.Priority >= prio {
			prio = n.Priority + 1
		}
	}
	for i := range next.Networks {
		if next.Networks[i].SSID == ssid {
			next.Networks[i].Priority = prio
		}
	}
	if err := writeWifi(s.path, next); err != nil {
		return err
	}
	s.cur = next
	return nil
}

// Forget removes ssid.
func (s *WifiStore) Forget(ssid string) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	next := s.cur
	kept := next.Networks[:0]
	for _, n := range next.Networks {
		if n.SSID != ssid {
			kept = append(kept, n)
		}
	}
	next.Networks = kept
	if err := writeWifi(s.path, next); err != nil {
		return err
	}
	s.cur = next
	return nil
}

// Erase deletes the file and forgets everything.
func (s *WifiStore) Erase() error {
	s.mu.Lock()
	defer s.mu.Unlock()
	if err := os.Remove(s.path); err != nil && !errors.Is(err, os.ErrNotExist) {
		return err
	}
	s.cur = wifiFile{}
	return nil
}

func writeWifi(path string, c wifiFile) error {
	data, err := json.MarshalIndent(c, "", "  ")
	if err != nil {
		return err
	}
	dir := filepath.Dir(path)
	tmp, err := os.CreateTemp(dir, ".wifi-*.json")
	if err != nil {
		return err
	}
	defer os.Remove(tmp.Name()) // no-op after a successful rename
	if err := tmp.Chmod(0o600); err != nil {
		tmp.Close()
		return err
	}
	if _, err := tmp.Write(append(data, '\n')); err != nil {
		tmp.Close()
		return err
	}
	if err := tmp.Sync(); err != nil {
		tmp.Close()
		return err
	}
	if err := tmp.Close(); err != nil {
		return err
	}
	if err := os.Rename(tmp.Name(), path); err != nil {
		return err
	}
	if d, err := os.Open(dir); err == nil {
		_ = d.Sync()
		d.Close()
	}
	return nil
}
