package netmgr

import (
	"io/fs"
	"os"
	"path/filepath"
	"sort"
	"strings"
)

// FS abstracts the parts of /sys the manager reads. Tests use a real tree
// under t.TempDir(); production uses the real /sys.
type FS interface {
	ReadDir(name string) ([]fs.DirEntry, error)
	ReadFile(name string) ([]byte, error)
	Lstat(name string) (fs.FileInfo, error)
}

// osFS is the production FS backed by the real filesystem.
type osFS struct{}

func (osFS) ReadDir(name string) ([]fs.DirEntry, error) { return os.ReadDir(name) }
func (osFS) ReadFile(name string) ([]byte, error)       { return os.ReadFile(name) }
func (osFS) Lstat(name string) (fs.FileInfo, error)     { return os.Lstat(name) }

// InterfaceInfo describes a physical interface discovered in /sys/class/net.
type InterfaceInfo struct {
	Name    string
	Kind    Kind
	Carrier bool
}

// Discover lists the physical interfaces under base (normally /sys/class/net).
// Loopback and virtual interfaces (no device symlink) are skipped. WiFi is
// detected by the wireless or phy80211 entry; anything else is ethernet.
func Discover(fsys FS, base string) ([]InterfaceInfo, error) {
	entries, err := fsys.ReadDir(base)
	if err != nil {
		return nil, err
	}
	var out []InterfaceInfo
	for _, e := range entries {
		name := e.Name()
		if name == "lo" {
			continue
		}
		if _, err := fsys.Lstat(filepath.Join(base, name, "device")); err != nil {
			continue // virtual interface: no backing device
		}
		info := InterfaceInfo{Name: name, Kind: KindEthernet}
		if _, err := fsys.Lstat(filepath.Join(base, name, "wireless")); err == nil {
			info.Kind = KindWiFi
		} else if _, err := fsys.Lstat(filepath.Join(base, name, "phy80211")); err == nil {
			info.Kind = KindWiFi
		}
		if b, err := fsys.ReadFile(filepath.Join(base, name, "carrier")); err == nil {
			info.Carrier = strings.TrimSpace(string(b)) == "1"
		}
		out = append(out, info)
	}
	sort.Slice(out, func(i, j int) bool { return out[i].Name < out[j].Name })
	return out, nil
}
