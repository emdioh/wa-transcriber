# Helpers for the SD card layout, shared by S00boot and S01data.
#
# The card layout (D18): p1 boot (FAT), p2 root A, p3 root B, p4 DATA. Which
# of p2/p3 is running does not matter to these helpers: they work on whatever
# block device the root filesystem is on and on its whole disk.
#
# /proc/mounts names the root filesystem "/dev/root" (the kernel's placeholder
# for the device given on the command line), so the real block device has to be
# resolved through /sys/dev/block/<major>:<minor>. Parsing the first field of
# /proc/mounts looks right on a desktop and fails on every image, which used to
# look like "wat.txt is ignored" and "open session db: ... unable to open
# database file".

# wat_rootdev prints the block device of the root filesystem, or nothing.
wat_rootdev() {
	majmin=$(awk '$5 == "/" { print $3; exit }' /proc/self/mountinfo 2>/dev/null)
	if [ -n "$majmin" ]; then
		dev=$(readlink -f "/sys/dev/block/$majmin" 2>/dev/null)
		if [ -n "$dev" ]; then
			echo "/dev/${dev##*/}"
			return 0
		fi
	fi
	# Last resort: the root= argument of the kernel command line.
	for arg in $(cat /proc/cmdline); do
		case "$arg" in
		root=/dev/*)
			echo "${arg#root=}"
			return 0
			;;
		esac
	done
	return 1
}

# wat_disk prints the whole disk of a partition: /dev/mmcblk0p2 -> /dev/mmcblk0,
# /dev/sda2 -> /dev/sda, /dev/nvme0n1p2 -> /dev/nvme0n1.
wat_disk() {
	case "$1" in
	/dev/mmcblk*p[0-9]*) echo "${1%p[0-9]*}" ;;
	/dev/*[0-9])
		d=${1%[0-9]*}
		case "$d" in
		*[0-9]p) d=${d%p} ;;
		esac
		echo "$d"
		;;
	*) return 1 ;;
	esac
}

# wat_part prints partition <n> of a disk: (/dev/mmcblk0, 3) -> /dev/mmcblk0p3.
# Partition numbers follow the card layout: 1 boot, 2 root A, 3 root B, 4 DATA.
wat_part() {
	case "$1" in
	/dev/mmcblk* | /dev/nvme*) echo "$1p$2" ;;
	*) echo "$1$2" ;;
	esac
}
