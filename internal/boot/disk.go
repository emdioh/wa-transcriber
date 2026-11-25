package boot

import (
	"context"
	"errors"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"time"
)

// rootPartition returns the device node the running root filesystem is on.
//
// The mount table does not name it: the kernel mounts what root= gave, and
// this image boots with /dev/mmcblk0p2 on the card and /dev/mmcblk1p2 on an
// installed eMMC, so the field is a major:minor pair and the node comes from
// /sys/dev/block/<major>:<minor>. Reading the device field of /proc/mounts
// instead ("/dev/root") is how a board once ignored wat.txt and then failed
// to open the session database with no useful message.
func (o Options) rootPartition() (string, error) {
	if b, err := o.ReadFile(o.Mounts); err == nil {
		if majmin := rootMajMin(string(b), o.Root); majmin != "" {
			link := filepath.Join(o.SysDir, "dev/block", majmin)
			target, err := filepath.EvalSymlinks(link)
			if err != nil {
				return "", fmt.Errorf("boot: %s: cannot resolve the root device: %w", link, err)
			}
			return o.DevDir + filepath.Base(target), nil
		}
	}
	// Last resort: root= on the kernel command line.
	if b, err := o.ReadFile(filepath.Join(o.ProcDir, "cmdline")); err == nil {
		for _, arg := range strings.Fields(string(b)) {
			if dev, ok := strings.CutPrefix(arg, "root="); ok && strings.HasPrefix(dev, "/dev/") {
				return dev, nil
			}
		}
	}
	return "", errors.New("boot: cannot find the block device of the root filesystem")
}

// rootMajMin finds the major:minor pair of the device mounted on mountPoint.
func rootMajMin(mountinfo, mountPoint string) string {
	for _, line := range strings.Split(mountinfo, "\n") {
		fields := strings.Fields(line)
		if len(fields) < 5 {
			continue
		}
		if unescapeMount(fields[4]) == mountPoint {
			return fields[2]
		}
	}
	return ""
}

// diskOf maps a partition device to its whole disk: /dev/mmcblk0p2 and
// /dev/nvme0n1p2 keep their "p", /dev/sda2 does not.
func diskOf(part string) (string, error) {
	base := filepath.Base(part)
	trim := func(n int) (string, error) {
		if n <= 0 || n >= len(base) {
			return "", fmt.Errorf("boot: %s is not a partition of a disk", part)
		}
		return part[:len(part)-len(base)+n], nil
	}
	switch {
	case strings.HasPrefix(base, "mmcblk"), strings.HasPrefix(base, "nvme"),
		strings.HasPrefix(base, "loop"), strings.HasPrefix(base, "md"):
		i := strings.LastIndexByte(base, 'p')
		if i < 0 || !allDigits(base[i+1:]) {
			return "", fmt.Errorf("boot: %s is not a partition of a disk", part)
		}
		return trim(i)
	default:
		i := len(base)
		for i > 0 && base[i-1] >= '0' && base[i-1] <= '9' {
			i--
		}
		return trim(i)
	}
}

// partOf is the inverse: (/dev/mmcblk0, 2) is /dev/mmcblk0p2 and
// (/dev/sda, 2) is /dev/sda2.
func partOf(disk string, n int) string {
	base := filepath.Base(disk)
	if strings.HasPrefix(base, "mmcblk") || strings.HasPrefix(base, "nvme") ||
		strings.HasPrefix(base, "loop") || strings.HasPrefix(base, "md") {
		return disk + "p" + strconv.Itoa(n)
	}
	return disk + strconv.Itoa(n)
}

func allDigits(s string) bool {
	if s == "" {
		return false
	}
	for _, r := range s {
		if r < '0' || r > '9' {
			return false
		}
	}
	return true
}

// isBlock reports whether path is a block device (or, in a test, a file that
// stands in for one).
func isBlock(path string) bool {
	fi, err := os.Stat(path)
	if err != nil {
		return false
	}
	return fi.Mode().IsRegular() || fi.Mode()&os.ModeDevice != 0
}

// waitBlock waits until a block device node appears: the kernel adds one
// asynchronously after the partition table changes.
func (o Options) waitBlock(path string) bool {
	deadline := time.Now().Add(o.Wait)
	for {
		if isBlock(path) {
			return true
		}
		if time.Now().After(deadline) {
			return false
		}
		time.Sleep(100 * time.Millisecond)
	}
}

// probeFilesystem reports what a block device carries, without blkid: the
// answer comes from the superblock bytes, which is all the decision below
// needs. A partition that carries something this cannot name is reported as
// unknown and is never formatted.
func (o Options) probeFilesystem(dev string) (kind, label string, err error) {
	f, err := os.Open(dev)
	if err != nil {
		return "", "", err
	}
	defer f.Close()
	buf := make([]byte, 8192)
	n, err := f.ReadAt(buf, 0)
	if err != nil && !errors.Is(err, io.EOF) {
		return "", "", err
	}
	// A partition with no filesystem yet reads as zeros, which a short read
	// on a device that has nothing to give is the same thing as.
	buf = buf[:n]

	switch {
	case len(buf) >= 1024+0x88 && buf[1024+0x38] == 0x53 && buf[1024+0x39] == 0xEF:
		// ext2/3/4: s_magic at 0x38 in the superblock, s_volume_name 16 bytes
		// at 0x78.
		name := buf[1024+0x78 : 1024+0x78+16]
		if i := strings.IndexByte(string(name), 0); i >= 0 {
			name = name[:i]
		}
		return "ext4", string(name), nil
	case len(buf) >= 86 && (string(buf[54:57]) == "FAT" || string(buf[82:85]) == "FAT"):
		return "fat", "", nil
	case len(buf) >= 4096+10 && string(buf[4086:4096]) == "SWAPSPACE2":
		return "swap", "", nil
	case allZero(buf):
		return "empty", "", nil
	default:
		return "unknown", "", nil
	}
}

func allZero(b []byte) bool {
	for _, c := range b {
		if c != 0 {
			return false
		}
	}
	return true
}

// mountBoot mounts the FAT partition (p1) read-only at /boot. The kernel
// does not do it: without it wat.txt is ignored and an image update cannot
// read the slot it is booting.
func (o Options) mountBoot(_ context.Context) error {
	if o.mounted(BootDir) {
		return nil
	}
	root, err := o.rootPartition()
	if err != nil {
		return err
	}
	disk, err := diskOf(root)
	if err != nil {
		return err
	}
	dev := partOf(disk, 1)
	if !o.waitBlock(dev) {
		return fmt.Errorf("boot: %s did not appear", dev)
	}
	return o.ensureMount(mountSpec{
		source: dev, target: BootDir, fstype: "vfat",
		flags: msReadOnly | msNoSuid | msNoDev | msNoExec,
	})
}

// mountData finds the data partition, creates it on a card that has none yet
// and mounts it at /data.
//
// It follows the rules of the shell script it replaces (S01data), because
// they are what makes a power cut during the first boot harmless:
//
//   - an existing DATA filesystem is never formatted and never touched;
//   - a partition that carries a filesystem this cannot name is left alone
//     (the device then runs without /data, which is a message in the log, not
//     a formatted card);
//   - the two root slots are never candidates, whatever their label: the
//     label is the identity and only partitions from 4 up are asked.
func (o Options) mountData(ctx context.Context) error {
	if o.mounted(DataDir) {
		return nil
	}
	root, err := o.rootPartition()
	if err != nil {
		return err
	}
	disk, err := diskOf(root)
	if err != nil {
		return err
	}

	dev := ""
	// A DATA partition that moved (a card with extra partitions, or one
	// prepared by hand) is still the data partition.
	for n := 4; n <= 8; n++ {
		p := partOf(disk, n)
		if !isBlock(p) {
			continue
		}
		if kind, label, err := o.probeFilesystem(p); err != nil {
			o.Log.Warn("cannot read a partition", "device", p, "err", err)
		} else if kind == "ext4" && label == DataLabel {
			dev = p
			break
		}
	}
	// The layout from before the second root slot kept data on p3. Only
	// consulted when there is no p4 at all, exactly like S01data: on the
	// current layout p3 is a root slot.
	if dev == "" && !isBlock(partOf(disk, 4)) {
		if p3 := partOf(disk, 3); isBlock(p3) {
			if kind, label, err := o.probeFilesystem(p3); err == nil && kind == "ext4" && label == DataLabel {
				dev = p3
				o.Log.Info("data partition on p3 (layout without a second root slot)")
			}
		}
	}
	if dev == "" {
		p4 := partOf(disk, 4)
		if !isBlock(p4) {
			// ",+,83": all free space, Linux type. sfdisk re-reads the table
			// itself through BLKPG, which works while the root filesystem on
			// that disk is mounted; --no-reread stops it from asking the
			// kernel to re-read a table it is using.
			o.Log.Info("creating the data partition", "disk", disk)
			if err := o.Run(ctx, ",+,83\n", "sfdisk", "--append", "--no-reread", disk); err != nil {
				return fmt.Errorf("boot: creating the data partition: %w", err)
			}
			// partx adds the new node to the running kernel. It answers with
			// an error when the node is already there, which is not a
			// failure here.
			if err := o.Run(ctx, "", "partx", "-a", "--nr", "4", disk); err != nil {
				o.Log.Debug("partx reported a problem, waiting for the node", "err", err)
			}
			if !o.waitBlock(p4) {
				return fmt.Errorf("boot: %s did not appear after creating it", p4)
			}
		}
		kind, label, err := o.probeFilesystem(p4)
		if err != nil {
			return fmt.Errorf("boot: %s: %w", p4, err)
		}
		switch {
		case kind == "ext4" && label == DataLabel:
			dev = p4
		case kind == "ext4":
			return fmt.Errorf("boot: %s carries ext4 without the %s label, leaving it alone", p4, DataLabel)
		case kind != "empty":
			return fmt.Errorf("boot: %s carries %s, leaving it alone", p4, kind)
		default:
			o.Log.Info("formatting the data partition", "device", p4)
			if err := o.Run(ctx, "", "mkfs.ext4", "-q", "-L", DataLabel, p4); err != nil {
				return fmt.Errorf("boot: formatting %s: %w", p4, err)
			}
			dev = p4
		}
	}
	return o.mountExt4(ctx, dev, DataDir)
}

// mountExt4 mounts the data filesystem, repairing it once if the mount fails:
// a power cut in the middle of a write can leave the journal half applied.
func (o Options) mountExt4(ctx context.Context, dev, target string) error {
	spec := mountSpec{
		source: dev, target: target, fstype: "ext4",
		flags: msNoAtime | msNoSuid | msNoDev,
	}
	err := o.ensureMount(spec)
	if err == nil {
		return nil
	}
	// e2fsck -p exits 0 on a healthy filesystem and does nothing, so a device
	// that mounts again after this was damaged, not merely unlucky.
	o.Log.Warn("cannot mount the data filesystem, checking it once", "device", dev, "err", err)
	if ferr := o.Run(ctx, "", "e2fsck", "-p", dev); ferr != nil {
		return fmt.Errorf("boot: %s: cannot mount it and e2fsck failed: %w", dev, ferr)
	}
	if err := o.ensureMount(spec); err != nil {
		return fmt.Errorf("boot: %s: %w", dev, err)
	}
	return nil
}
