package imageupdate

import (
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"strconv"
	"strings"
)

// Layout describes where the pieces of the A/B layout are on this system.
// Every path is injected so the package runs on a dev machine or in a test
// with files instead of partitions. main builds it with DeviceLayout.
type Layout struct {
	// Root is the mount point of the running root ("/" on a board).
	Root string
	// BootDir is the mounted boot partition. It is read-only on a board and
	// the caller remounts it rw for the duration of a Switch (see main).
	BootDir string
	// BootFile is the file inside BootDir that names the root partition:
	// extlinux/extlinux.conf on the BeagleBone Black, cmdline.txt on the Pi.
	BootFile string
	// DataDir holds image-boot.json (/data).
	DataDir string
	// SysBlock is the parent of the per-partition sysfs directories
	// (/sys/class/block in DeviceLayout).
	//
	// The device node of a partition is l.DevDir + its sysfs name. DevDir
	// exists because a test writes partitions as files somewhere in a temp
	// directory while a board has them in /dev; the sysfs names themselves
	// (mmcblk0p2, sda2) are the real ones either way.
	DevDir   string
	SysBlock string
	// Slots is the device name of each root partition, in layout order. The
	// "p" is part of the name because it belongs to the disk: mmcblk0p2 comes
	// from disk mmcblk0 and name p2, sda2 from disk sda and name 2.
	Slots [2]string
}

// SlotState says what is in one root slot.
type SlotState struct {
	Path string // the partition device, e.g. /dev/mmcblk0p2
	Side string // "A" or "B"
	// Size is the partition size in bytes, 0 when it could not be read.
	Size int64
}

// Slots is what the running system knows about the two root slots.
type Slots struct {
	Disk    string       // the whole disk, e.g. mmcblk0 (a name, not a path)
	Current SlotState    // the partition / is mounted from
	Other   SlotState    // the slot an image update writes
	Both    [2]SlotState // A and B, in layout order
}

// ErrNoSlots is returned when the two root slots cannot be told apart: an
// old-layout device (one root, a data partition in the second slot), a
// container, a root that is not a partition. It is the answer the UI shows as
// "reflash once to enable image updates".
var ErrNoSlots = errors.New("imageupdate: no second root slot: this layout cannot update the image in place")

// FindSlots works out which of the two root partitions is running and which
// one an update would write.
//
// The running root is identified by its device number: stat("/") gives the
// device of the mounted filesystem, and the partition under
// <SysBlock>/<name>/dev carries the same major:minor. The disk is that
// partition's parent: the same device number minus the partition, going by
// the name (mmcblk0p2 → mmcblk0), so sd*, vd*, nvme* and mmcblk* all work.
//
// It refuses with ErrNoSlots when the root is not one of the two configured
// partitions (old layout, overlay, container) or when the partitions are not
// on the same disk: an update must never write something whose identity is a
// guess.
func (l Layout) FindSlots() (Slots, error) {
	var zero Slots
	if l.Root == "" || l.SysBlock == "" || l.Slots[0] == "" || l.Slots[1] == "" || l.Slots[0] == l.Slots[1] {
		return zero, ErrNoSlots
	}
	fi, err := os.Stat(l.Root)
	if err != nil {
		// Not an error with the system to report: it is a root whose identity
		// is unknown, which is the same answer as an old layout.
		return zero, ErrNoSlots
	}
	dev, ok := fsDevice(fi)
	if !ok {
		return zero, ErrNoSlots
	}
	dirs, err := filepath.Glob(filepath.Join(l.SysBlock, "*"))
	if err != nil {
		return zero, fmt.Errorf("imageupdate: list %s: %w", l.SysBlock, err)
	}
	var names []string
	for _, dir := range dirs {
		if readDev(filepath.Join(dir, "dev")) == dev {
			names = append(names, filepath.Base(dir))
		}
	}
	// On a board a partition has one sysfs entry; a bind mount or a second
	// entry would make "the" running partition ambiguous.
	if len(names) != 1 {
		return zero, ErrNoSlots
	}
	running := names[0]

	parent, ok := parentDisk(running, l.Slots)
	if !ok {
		return zero, ErrNoSlots
	}
	out := Slots{Disk: parent}
	for i, suffix := range l.Slots {
		name := parent + suffix
		side := string(rune('A' + i))
		st := SlotState{Path: l.DevDir + name, Side: side, Size: partitionSize(l.DevDir + name)}
		out.Both[i] = st
		if name == running {
			out.Current = st
		} else {
			if out.Other.Path != "" {
				// Both slots running: impossible, refuse rather than pick.
				return zero, ErrNoSlots
			}
			out.Other = st
		}
	}
	if out.Current.Path == "" || out.Other.Path == "" {
		return zero, ErrNoSlots
	}
	return out, nil
}

// parentDisk finds the disk of a running partition by stripping the partition
// suffix of one of the configured slots. It only accepts a name that belongs
// to the layout: anything else (a USB stick, a leftover mount) is not a slot.
func parentDisk(running string, slots [2]string) (string, bool) {
	for _, suffix := range slots {
		if strings.HasSuffix(running, suffix) && len(running) > len(suffix) {
			return strings.TrimSuffix(running, suffix), true
		}
	}
	return "", false
}

// readDev parses a sysfs "dev" file ("179:2") into the device number stat
// reports.
func readDev(path string) uint64 {
	b, err := os.ReadFile(path)
	if err != nil {
		return 0
	}
	major, minor, ok := strings.Cut(strings.TrimSpace(string(b)), ":")
	if !ok {
		return 0
	}
	m, err1 := strconv.ParseUint(major, 10, 32)
	n, err2 := strconv.ParseUint(minor, 10, 32)
	if err1 != nil || err2 != nil {
		return 0
	}
	return mkdev(m, n)
}

// partitionSize reads the size of a block device in bytes (0 when unknown:
// a file used in a test, a device that just vanished).
func partitionSize(path string) int64 {
	f, err := os.Open(path)
	if err != nil {
		return 0
	}
	defer f.Close()
	size, err := f.Seek(0, 2)
	if err != nil || size < 0 {
		return 0
	}
	return size
}
