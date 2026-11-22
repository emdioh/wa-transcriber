// Package flash installs the running SD card onto the board's internal
// flash (M11, ARCHITECTURE.md section 11): a block-for-block clone of the
// card's MBR and its three system partitions (boot, root A, root B) onto the
// eMMC, then the read-back that says whether the eMMC is bootable yet.
//
// The eMMC is found by rule, never by guess (Target): the parent disk of
// /sys/class/block/* whose device driver is MMC, whose removable flag is 0,
// that is not the disk the running root came from and that is at least as
// big as the part of the card being copied. Zero or two such disks is a
// refusal with the list of what was seen, and a board without internal flash
// (the Raspberry Pi 2) gets exactly the one line ErrNoTarget carries.
//
// Nothing here talks to a kernel interface by itself: the sysfs tree, the
// device directory and the running root are passed in, so the whole package
// runs on temp files in a test — a regular file stands in for the eMMC and a
// small hand-made sysfs tree for /sys/class/block.
//
// The card is the source and always stays intact. A clone that fails or is
// interrupted leaves a half-written eMMC that is simply not bootable yet and
// a card that still boots the device; running the clone again completes it,
// because every byte is a copy, not an increment. The migration marker
// (/boot/wat-migrate) is written last, so an interrupted clone can never
// make the new system copy /data from a partition that does not exist on it
// yet.
package flash
