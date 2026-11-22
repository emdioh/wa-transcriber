package flash

import (
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"strconv"
	"strings"
)

// ErrNoTarget is the one answer a board without internal flash gets (the
// Raspberry Pi 2: its only MMC disk is the card it runs from). The UI shows
// it as a single line, so it is a sentence, not an error chain.
var ErrNoTarget = errors.New("flash: no internal storage found")

// Target is the internal flash the card would be installed on.
type Target struct {
	// Name is the sysfs name of the disk, e.g. "mmcblk1".
	Name string
	// Device is the device node, <DevDir><Name>: "/dev/mmcblk1".
	Device string
	// Size is the disk size in bytes, 0 when it could not be read.
	Size int64
	// Removable is the sysfs flag of the disk, reported as the refusal line
	// shows it. It is *not* what tells the SD slot from the eMMC on the
	// BeagleBone Black: both report 0 there (OMAP's mmc driver does not mark
	// the SD slot removable), so device/type == "MMC" plus "not the running
	// disk" is what decides (see disks).
	Removable bool
	// Running is the disk the running root came from, for the log line.
	Running string
}

// Candidate is one disk the rule looked at, whether or not it was accepted.
// It is what a refusal prints: the owner has to be able to see why the
// answer was "none" or "two".
type Candidate struct {
	Name      string
	Type      string // /sys/class/block/<disk>/device/type: "MMC", "SCSI", ...
	Removable bool
	Size      int64
	Running   bool
	// Why it was not accepted ("" = accepted).
	Why string
}

func (c Candidate) String() string {
	s := fmt.Sprintf("%s type=%q removable=%v size=%s",
		c.Name, c.Type, c.Removable, sizeMB(c.Size))
	if c.Running {
		s += " running-disk"
	}
	if c.Why != "" {
		s += " (" + c.Why + ")"
	}
	return s
}

// Sys describes the machine the target is looked for on. Zeros are the board
// defaults, so a test sets all three and a caller on a board only the size it
// needs.
type Sys struct {
	// SysBlock is /sys/class/block (or a directory of the same shape).
	SysBlock string
	// DevDir is "/dev/" on a board; a test points it at its temp files.
	DevDir string
	// Root is the mount point that identifies the running disk ("/").
	Root string
	// Need is the number of bytes the clone writes: the target has to be at
	// least this big, or the clone would run out of disk halfway.
	Need int64
}

func (s Sys) sysBlock() string {
	if s.SysBlock != "" {
		return s.SysBlock
	}
	return "/sys/class/block"
}

func (s Sys) devDir() string {
	if s.DevDir != "" {
		return s.DevDir
	}
	return "/dev/"
}

func (s Sys) root() string {
	if s.Root != "" {
		return s.Root
	}
	return "/"
}

// Target finds the internal flash by rule: the parent disk of
// <SysBlock>/* whose <disk>/device/type is MMC, that is not the disk the
// running root is on, and that is at least Need bytes big.
//
// The device type is the primary discriminator. The removable flag is not: on
// the BeagleBone Black both the SD slot and the eMMC report removable=0, so a
// rule that leaned on it would see two identical candidates (or, worse, pick
// the card). The running disk is excluded by device number, which works on
// every board; the removable flag is only a last resort for a disk of type
// MMC that is neither the root nor a real eMMC, and it is what the refusal
// line prints beside the type so the owner can see what was found.
//
// It refuses when there is none (ErrNoTarget) or more than one, and the error
// lists every disk it looked at.
//
// It never returns an error and a target at the same time: an identity that
// is a guess is refused, because the next step writes a whole disk.
func (s Sys) Target() (Target, error) {
	cards, err := disks(s)
	if err != nil {
		return Target{}, err
	}
	var got []Target
	for _, c := range cards {
		if c.Why == "" {
			got = append(got, Target{
				Name: c.Name, Device: s.devDir() + c.Name, Size: c.Size,
				Removable: c.Removable,
			})
		}
	}
	switch len(got) {
	case 0:
		return Target{}, fmt.Errorf("%w: looked at %s", ErrNoTarget, candidates(cards))
	case 1:
		if got[0].Size <= 0 {
			// A size that cannot be read is not a disk to write blind.
			return Target{}, fmt.Errorf("%w: %s has no readable size", ErrNoTarget, got[0].Device)
		}
		return got[0], nil
	default:
		return Target{}, fmt.Errorf("flash: %d internal storage devices found, refusing to pick one: %s",
			len(got), candidates(cards))
	}
}

// candidates renders the list a refusal prints.
func candidates(cards []Candidate) string {
	if len(cards) == 0 {
		return "no block devices"
	}
	parts := make([]string, 0, len(cards))
	for _, c := range cards {
		parts = append(parts, c.String())
	}
	return strings.Join(parts, ", ")
}

// disks lists every whole disk under SysBlock with the rule's verdict on it.
// The list itself is best effort — a disk that vanished while it was being
// read is skipped — but a SysBlock that cannot be read at all is an error:
// "no internal storage" is the wrong answer for a missing /sys.
func disks(s Sys) ([]Candidate, error) {
	ents, err := os.ReadDir(s.sysBlock())
	if err != nil {
		return nil, fmt.Errorf("flash: reading %s: %w", s.sysBlock(), err)
	}
	running := runningDisks(s.sysBlock(), s.root())
	var out []Candidate
	for _, e := range ents {
		dir := filepath.Join(s.sysBlock(), e.Name())
		if !isWholeDisk(dir) {
			continue
		}
		c := Candidate{
			Name:      e.Name(),
			Type:      readString(filepath.Join(dir, "device", "type")),
			Removable: readString(filepath.Join(dir, "removable")) == "1",
			Size:      readSize(filepath.Join(dir, "size")),
		}
		c.Running = running[e.Name()]
		switch {
		case c.Type != "MMC":
			c.Why = "not an MMC device"
		case c.Running:
			c.Why = "this is the running disk"
		case c.Removable:
			c.Why = "removable"
		case s.Need > 0 && c.Size > 0 && c.Size < s.Need:
			c.Why = fmt.Sprintf("smaller than the %s to be copied", sizeMB(s.Need))
		}
		out = append(out, c)
	}
	return out, nil
}

// isWholeDisk reports whether dir is a disk and not a partition: sysfs gives
// both a removable flag, only a partition has a "partition" file.
func isWholeDisk(dir string) bool {
	_, err := os.Stat(filepath.Join(dir, "partition"))
	return err != nil
}

// runningDisks finds the disk the running root is on: stat(root) gives the
// device number of the filesystem, a partition of <SysBlock> carries the same
// number in its "dev" file, and the disk is that partition's parent — the
// name minus its partition suffix, so mmcblk0p2 → mmcblk0 and sda2 → sda.
func runningDisks(sysBlock, root string) map[string]bool {
	out := map[string]bool{}
	fi, err := os.Stat(root)
	if err != nil {
		return out
	}
	dev, ok := fsDevice(fi)
	if !ok {
		return out
	}
	ents, err := os.ReadDir(sysBlock)
	if err != nil {
		return out
	}
	for _, e := range ents {
		dir := filepath.Join(sysBlock, e.Name())
		if _, err := os.Stat(filepath.Join(dir, "partition")); err != nil {
			continue
		}
		if readDev(filepath.Join(dir, "dev")) != dev {
			continue
		}
		if disk := parentDisk(e.Name()); disk != "" {
			out[disk] = true
		}
	}
	return out
}

// parentDisk strips a partition suffix: mmcblk0p2 → mmcblk0, nvme0n1p2 →
// nvme0n1, sda2 → sda (the same rule as sd.sh's wat_disk).
func parentDisk(name string) string {
	if i := strings.LastIndex(name, "p"); i > 0 && isAllDigits(name[i+1:]) {
		// mmcblk0p2, nvme0n1p2: "p" separates disk from partition.
		return name[:i]
	}
	j := len(name)
	for j > 0 && name[j-1] >= '0' && name[j-1] <= '9' {
		j--
	}
	if j == len(name) || j == 0 {
		return "" // no partition number: a disk, not a partition
	}
	return name[:j]
}

func isAllDigits(s string) bool {
	if s == "" {
		return false
	}
	for i := 0; i < len(s); i++ {
		if s[i] < '0' || s[i] > '9' {
			return false
		}
	}
	return true
}

// partitionDev is the device node of partition n on diskName: mmcblk0p4,
// sda4, nvme0n1p4. It is what the migration marker names.
func partitionDev(diskName string, n int) string {
	if diskName == "" {
		return ""
	}
	switch {
	case strings.HasPrefix(diskName, "mmcblk"), strings.HasPrefix(diskName, "nvme"), strings.HasPrefix(diskName, "loop"):
		return fmt.Sprintf("%sp%d", diskName, n)
	}
	return fmt.Sprintf("%s%d", diskName, n)
}

// readString reads a one-line sysfs attribute ("" when missing).
func readString(path string) string {
	b, err := os.ReadFile(path)
	if err != nil {
		return ""
	}
	return strings.TrimSpace(string(b))
}

// readSize reads a sysfs "size" file, which is in 512-byte sectors.
func readSize(path string) int64 {
	n, err := strconv.ParseInt(readString(path), 10, 64)
	if err != nil || n <= 0 {
		return 0
	}
	return n * 512
}

// readDev parses a sysfs "dev" file ("179:2").
func readDev(path string) uint64 {
	major, minor, ok := strings.Cut(readString(path), ":")
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

// sizeMB renders a byte count for the refusals ("4 GB 512 MB").
func sizeMB(n int64) string {
	if n <= 0 {
		return "unknown"
	}
	const mb = 1 << 20
	if n < mb {
		return fmt.Sprintf("%d kB", (n+1023)/1024)
	}
	if n%(1<<30) == 0 {
		return fmt.Sprintf("%d GB", n>>30)
	}
	return fmt.Sprintf("%d MB", n/mb)
}
