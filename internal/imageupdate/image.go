package imageupdate

import (
	"fmt"
	"regexp"
)

// LayoutVersion is the A/B partition table this package understands: p1 boot,
// p2 root A, p3 root B, p4 data. A release manifest names the version it needs
// and a device on an older table has to reflash once, which is the only way to
// change a partition table anyway.
const LayoutVersion = 2

// Image is one board image in the release manifest ("images" block). It is a
// separate type from update.Asset because it is not executable: no ELF header,
// no arch, just a file to write into a partition.
type Image struct {
	File   string `json:"file"`
	SHA256 string `json:"sha256"`
	Size   int64  `json:"size"`
	// Layout is the partition table version the image needs (0 = any, for a
	// manifest written before the A/B layout existed).
	Layout int `json:"layout,omitempty"`
}

// filePattern keeps a manifest image name from being a path: the same reason
// update validates its asset keys.
var filePattern = regexp.MustCompile(`^[A-Za-z0-9][A-Za-z0-9._-]*$`)

// Validate checks one image entry of a manifest. It runs on every image, even
// the other board's, because the manifest is signed as a whole.
func (i Image) Validate(key string) error {
	if !filePattern.MatchString(i.File) || len(i.File) > 128 {
		return fmt.Errorf("imageupdate: image %q has file %q", key, i.File)
	}
	if !isHex(i.SHA256) || len(i.SHA256) != 64 {
		return fmt.Errorf("imageupdate: image %q has sha256 %q", key, i.SHA256)
	}
	if i.Size <= 0 {
		return fmt.Errorf("imageupdate: image %q has size %d", key, i.Size)
	}
	return nil
}

// NeedsLayout reports whether this device can install the image: a manifest
// that wants a newer table than LayoutVersion is refused before the download,
// with the line the UI shows.
func (i Image) NeedsLayout() bool { return i.Layout > LayoutVersion }
