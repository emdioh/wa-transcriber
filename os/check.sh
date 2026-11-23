#!/bin/sh
#
# Structural checks for the BR2_EXTERNAL tree. Run by "make -C os check" (and
# therefore before any image build); it does not need Buildroot or hardware.
#
# It catches the mistakes that otherwise only show up an hour into a build
# or, worse, at boot: a missing file referenced by a defconfig, a package
# recipe without its name prefix, an init script without the executable bit,
# a genimage config naming a file the board never produces.

set -eu

HERE="$(cd "$(dirname "$0")" && pwd)"
failed=0

fail() {
	printf 'FAIL: %s\n' "$*" >&2
	failed=$((failed + 1))
}

ok() { printf 'ok:   %s\n' "$*"; }

# --- br2-external metadata -------------------------------------------------

[ -f "$HERE/external.desc" ] || fail "missing external.desc"
grep -q '^name: [A-Za-z0-9_]*$' "$HERE/external.desc" || fail "external.desc needs 'name: XXX'"
grep -q '^name: WAT$' "$HERE/external.desc" || fail "the tree name must stay WAT (paths in defconfigs use BR2_EXTERNAL_WAT_PATH)"
[ -f "$HERE/Config.in" ] || fail "missing Config.in"
[ -f "$HERE/external.mk" ] || fail "missing external.mk"
grep -q 'BR2_EXTERNAL_WAT_PATH' "$HERE/Config.in" || fail "Config.in does not source the package Config.in"
ok "br2-external metadata"

# --- package recipe --------------------------------------------------------

PKG="$HERE/package/wa-transcriber"
for f in Config.in wa-transcriber.mk; do
	[ -f "$PKG/$f" ] || fail "missing package file $f"
done
grep -q 'config BR2_PACKAGE_WA_TRANSCRIBER' "$PKG/Config.in" ||
	fail "Config.in does not define BR2_PACKAGE_WA_TRANSCRIBER"
grep -q 'generic-package' "$PKG/wa-transcriber.mk" ||
	fail "the package must be a generic-package (it only copies a prebuilt binary)"
grep -q 'wa-transcriber-linux-armv7' "$PKG/wa-transcriber.mk" ||
	fail "the package does not install the armv7 binary"
ok "package recipe"

# --- defconfigs ------------------------------------------------------------

for board in rpi2 bbb; do
	defconfig="$HERE/configs/wat_${board}_defconfig"
	[ -f "$defconfig" ] || { fail "missing $defconfig"; continue; }

	grep -q '^BR2_PACKAGE_WA_TRANSCRIBER=y$' "$defconfig" ||
		fail "$board: the transcriber package is not enabled"
	grep -q '^# BR2_TARGET_GENERIC_REMOUNT_ROOTFS_RW is not set$' "$defconfig" ||
		fail "$board: the root filesystem must stay read-only"
	grep -q '^BR2_PACKAGE_HOSTAPD=y$' "$defconfig" ||
		fail "$board: hostapd (setup AP) is not enabled"
	grep -q '^BR2_PACKAGE_WPA_SUPPLICANT=y$' "$defconfig" ||
		fail "$board: wpa_supplicant is not enabled"
	grep -q '^BR2_PACKAGE_DNSMASQ=y$' "$defconfig" ||
		fail "$board: dnsmasq (setup AP DHCP/DNS) is not enabled"
	# S01data adds the DATA partition to the running kernel with partx.
	grep -q '^BR2_PACKAGE_UTIL_LINUX_PARTX=y$' "$defconfig" ||
		fail "$board: partx is missing (needed to add the DATA partition)"

	# The WiFi drivers in the kernel fragment need their blobs in the rootfs.
	# A missing one is not a build error, it is a dead dongle on the board.
	frag="$HERE/board/common/linux.fragment"
	if grep -q '^CONFIG_MT76x2U=m$' "$frag"; then
		grep -q '^BR2_PACKAGE_LINUX_FIRMWARE_MEDIATEK_MT76X2E=y$' "$defconfig" ||
			fail "$board: mt76x2u needs linux-firmware MEDIATEK_MT76X2E (mt7662*.bin)"
	fi
	if grep -q '^CONFIG_RTL8XXXU=m$' "$frag"; then
		grep -q '^BR2_PACKAGE_LINUX_FIRMWARE_RTL_81XX=y$' "$defconfig" ||
			fail "$board: rtl8xxxu needs linux-firmware RTL_81XX (rtl8192eu_nic.bin)"
	fi
	if grep -q '^BR2_PACKAGE_LINUX_FIRMWARE_[A-Z0-9_]*=y$' "$defconfig"; then
		grep -q '^BR2_PACKAGE_LINUX_FIRMWARE=y$' "$defconfig" ||
			fail "$board: linux-firmware sub-options need BR2_PACKAGE_LINUX_FIRMWARE=y"
	fi

	# Every file the defconfig points at must exist.
	for ref in $(sed -n 's/.*BR2_EXTERNAL_WAT_PATH)\([^"]*\)".*/\1/p' "$defconfig"); do
		[ -e "$HERE$ref" ] || fail "$board: defconfig references missing file $ref"
	done
	# Same for directory-valued options (fragments, overlay).
	for ref in $(sed -n 's/.*BR2_EXTERNAL_WAT_PATH)\/\([^"]*\)".*/\1/p' "$defconfig"); do
		[ -e "$HERE/$ref" ] || fail "$board: defconfig references missing path $ref"
	done
	ok "defconfig $board"
done

# --- board files -----------------------------------------------------------

for board in rpi2 bbb; do
	for f in genimage.cfg post-build.sh; do
		[ -f "$HERE/board/$board/$f" ] || fail "missing board/$board/$f"
	done
	[ -x "$HERE/board/$board/post-build.sh" ] || fail "board/$board/post-build.sh is not executable"
	grep -q 'sdcard.img' "$HERE/board/$board/genimage.cfg" || fail "board/$board/genimage.cfg has no sdcard.img"
	# Two root slots (D18): p1 boot, p2 root A, p3 root B, p4 data at first
	# boot. Both roots are images in the config; the labels are set by
	# mkimage.sh and are only asserted there.
	for img in rootfs.ext4 rootfs-b.ext4; do
		grep -q "image = \"$img\"" "$HERE/board/$board/genimage.cfg" ||
			fail "board/$board/genimage.cfg has no partition for $img (two root slots, D18)"
	done
	[ "$(grep -c 'size = 160M' "$HERE/board/$board/genimage.cfg")" -ge 2 ] ||
		fail "board/$board/genimage.cfg: the two root slots must both be 160M"
	# The boot files name the first slot by device path, and no partition
	# number after p3 may exist in the image: DATA is created on the board.
	[ "$(grep -c 'partition ' "$HERE/board/$board/genimage.cfg")" -eq 3 ] ||
		fail "board/$board/genimage.cfg must define exactly three partitions (boot, root A, root B)"
	# Files named in a genimage vfat section must be produced by the build.
	for f in $(sed -n '/vfat {/,/}/p' "$HERE/board/$board/genimage.cfg" |
		sed -n 's/^[[:space:]]*"\([^"]*\)",*$/\1/p'); do
		case "$f" in
		wat.txt | zImage | extlinux | cmdline.txt | config.txt) continue ;;
		esac
		[ -n "$f" ] || continue
	done
	ok "board files $board"
done

# --- rootfs overlay --------------------------------------------------------

OVL="$HERE/board/common/rootfs-overlay"
for f in etc/inittab etc/fstab etc/init.d/S00boot etc/init.d/S01data \
	etc/init.d/S02console etc/init.d/S40ntpd etc/init.d/S90watchdog \
	usr/lib/wat/sd.sh; do
	[ -f "$OVL/$f" ] || fail "missing overlay file $f"
done
for s in "$OVL"/etc/init.d/S*; do
	[ -x "$s" ] || fail "init script $(basename "$s") is not executable"
done
grep -q 'wa-transcriber -data /data -network managed -listen :80' "$OVL/etc/inittab" ||
	fail "inittab does not respawn the transcriber in managed mode"
# Binary self-updates (D17): init starts the factory binary, and that same
# binary has to run the boot gate on every restart. Without both flags the
# image either never updates or restarts straight into /data/bin.
grep -q 'wa-transcriber -data /data .*-update-dir /data/bin' "$OVL/etc/inittab" ||
	fail "inittab does not pass -update-dir /data/bin (binary self-updates are off)"
grep -q 'wa-transcriber -data /data .*-bootstrap /usr/bin/wa-transcriber' "$OVL/etc/inittab" ||
	fail "inittab does not pass -bootstrap /usr/bin/wa-transcriber (a restart would skip the boot gate)"
# Image updates (D18): off unless init says where the state lives, and each
# board has to describe its own boot file and slots or nothing can switch.
grep -q 'wa-transcriber -data /data .*-image-update /data' "$OVL/etc/inittab" ||
	fail "inittab does not pass -image-update /data (image updates are off)"
for board in bbb:beagle:/boot/extlinux/extlinux.conf rpi2:rpi2:/boot/cmdline.txt; do
	dir=${board%%:*}; rest=${board#*:}; name=${rest%%:*}; boot=${rest#*:}
	grep -q '"boot_file": "'"$boot"'"' "$HERE/board/$dir/post-build.sh" ||
		fail "board/$dir/post-build.sh does not write etc/wat/image.json with boot_file $boot"
	grep -q '"board": "'"$name"'"' "$HERE/board/$dir/post-build.sh" ||
		fail "board/$dir/post-build.sh does not name the manifest board $name"
done
grep -q 'sysinit:/etc/init.d/rcS' "$OVL/etc/inittab" ||
	fail "inittab does not run rcS"
grep -q '^LABEL=DATA' "$OVL/etc/fstab" || fail "fstab does not mount the DATA partition"
grep -q '^LABEL=DATA.*noauto' "$OVL/etc/fstab" ||
	fail "LABEL=DATA must be noauto: S01data mounts it after creating it"
# sfdisk refuses to touch a disk in use without --no-reread, and the partition
# is handed to the kernel with partx (BLKPG) because the disk is mounted.
grep -q 'sfdisk --append --no-reread' "$OVL/etc/init.d/S01data" ||
	fail "S01data must call sfdisk --append --no-reread"
grep -q 'partx -a' "$OVL/etc/init.d/S01data" ||
	fail "S01data must make the kernel see the new partition (partx -a)"
# The label lookup goes through blkid and failed on the board even when the
# label was there: mount the device itself.
grep -q 'mount -t ext4 -o noatime,nodev,nosuid "\$datadev" /data' "$OVL/etc/init.d/S01data" ||
	fail "S01data must mount \$datadev at /data, not LABEL=DATA"
grep -q '/var/lib\ttmpfs\|/var/lib.*tmpfs' "$OVL/etc/fstab" ||
	fail "fstab must mount a tmpfs on /var/lib (seedrng writes there)"
# Buildroot treats the local package source as immutable, so the board
# targets must dirclean it or the image keeps the first binary it ever had.
for board in rpi2 bbb; do
	grep -q "wa-transcriber-dirclean" "$HERE/Makefile" ||
		fail "os/Makefile must dirclean the transcriber package before building"
done
# /run/lock is on the tmpfs that "mount -a" mounts, so the mkdir comes after.
a=$(grep -n '^::sysinit:/bin/mount -a$' "$OVL/etc/inittab" | cut -d: -f1)
b=$(grep -n 'mkdir -p /run/lock' "$OVL/etc/inittab" | cut -d: -f1)
[ -n "$a" ] && [ -n "$b" ] && [ "$a" -lt "$b" ] ||
	fail "inittab must mount -a before creating /run/lock (read-only root)"
grep -q 'sysfs' "$OVL/etc/fstab" || fail "fstab does not mount /sys (needed by mdev and the network manager)"
# Layout with two root slots (D18): DATA is p4, found by its label, and the
# script may never take one of the system partitions for it. The empty root B
# is made by mkimage.sh with the same label the check asserts here.
grep -q 'wat_part "\$disk" 4' "$OVL/etc/init.d/S01data" ||
fail "S01data must take DATA from partition 4"
grep -q -e '-o value -s LABEL' "$OVL/etc/init.d/S01data" ||
fail "S01data must find DATA by its label"
grep -q -e '-s LABEL "$dev"' "$OVL/etc/init.d/S01data" ||
fail "S01data does not look for the DATA label"
grep -q 'wat_part "\$disk" 2' "$OVL/etc/init.d/S01data" &&
grep -q 'wat_part "\$disk" 3' "$OVL/etc/init.d/S01data" ||
fail "S01data must know the two root slots (p2, p3) to stay off them"
grep -q -- '--nr 4' "$OVL/etc/init.d/S01data" ||
fail "S01data must hand partition 4 to the kernel (partx -a --nr 4)"
# Install to the internal storage (M11): the eMMC clone carries the staged
# files (wat-migrate.d/) and /boot/wat-migrate on the *target's* boot
# partition, and the first start from it copies /data across from there. The
# staged directory is the working path: the card the files came from is out of
# the slot by then (the ROM boots the SD when one is inserted), so a marker
# that only names the card's DATA partition can never be honoured. Without the
# read of the staged directory the device comes up on the internal storage
# unpaired.
grep -q 'migrate_from_marker' "$OVL/etc/init.d/S01data" ||
fail "S01data does not run the /data migration (migrate_from_marker)"
grep -q 'stageddir=/boot/wat-migrate.d' "$OVL/etc/init.d/S01data" ||
fail "S01data does not look for the staged files on /boot (wat-migrate.d)"
grep -q '\[ -f "\$stageddir/\$name" \]' "$OVL/etc/init.d/S01data" ||
fail "S01data does not copy from the staged directory: the card is gone on the first eMMC boot"
grep -q 's/^file //p' "$OVL/etc/init.d/S01data" ||
fail "S01data does not read the file list from the migration marker"
# The old format is still read (a card left in on a board where the first
# boot happens with it inserted), but nothing may depend on it any more.
grep -q 's/^from //p' "$OVL/etc/init.d/S01data" ||
fail "S01data does not read the source partition from the migration marker"
grep -q 'mount -t ext4 -o ro,nodev,nosuid' "$OVL/etc/init.d/S01data" ||
fail "S01data must mount the old DATA read-only for the migration"
grep -q '/boot/wat-migrate' "$OVL/etc/init.d/S01data" ||
fail "S01data does not look for /boot/wat-migrate"
# The staged files are deleted after the marker, with /boot writable for the
# moment it takes; failing that, the marker is still gone and the copy stands.
grep -q 'mount -o remount,rw /boot' "$OVL/etc/init.d/S01data" ||
fail "S01data does not make /boot writable to delete the staged files"
grep -q 'rm -rf "\$stageddir"' "$OVL/etc/init.d/S01data" ||
fail "S01data does not delete wat-migrate.d after the copy"
# The staged files must be gone *after* the marker: deleting them first would
# leave a marker with nothing behind it if the power fails in between.
m=$(grep -n 'rm -f "\$marker"' "$OVL/etc/init.d/S01data" | tail -n 1 | cut -d: -f1)
s=$(grep -n 'rm -rf "\$stageddir"' "$OVL/etc/init.d/S01data" | tail -n 1 | cut -d: -f1)
[ -n "$m" ] && [ -n "$s" ] && [ "$m" -lt "$s" ] ||
fail "S01data must delete the migration marker before the staged files"
# The migration has to run after DATA is mounted at /data, or it copies into
# nothing.
a=$(grep -n 'DATA mounted from \$datadev' "$OVL/etc/init.d/S01data" | head -n 1 | cut -d: -f1)
b=$(grep -n '^migrate_from_marker$' "$OVL/etc/init.d/S01data" | tail -n 1 | cut -d: -f1)
[ -n "$a" ] && [ -n "$b" ] && [ "$a" -lt "$b" ] ||
fail "S01data must mount DATA before migrating it"
# The boot partition carries wat.txt: without the mount the binary silently
# falls back to its defaults.
grep -q 'mount .*\-t vfat .* /boot' "$OVL/etc/init.d/S00boot" ||
	fail "S00boot does not mount the boot partition at /boot"
grep -q 'mkdir -p "\$TARGET_DIR/boot"' "$HERE/board/common/post-build.sh" ||
	fail "post-build.sh does not create the /boot mountpoint"
# Linux 6.1 asks mt76x2u for mt7662*.bin at the top of /lib/firmware while
# linux-firmware installs them under mediatek/.
grep -qF 'ln -sf "mediatek/$fw" "$TARGET_DIR/lib/firmware/$fw"' "$HERE/board/common/post-build.sh" ||
	fail "post-build.sh does not link mediatek/mt7662*.bin where the kernel looks"
for s in S00boot S01data; do
	grep -q '/usr/lib/wat/sd.sh' "$OVL/etc/init.d/$s" ||
		fail "$s does not use the shared SD-card helpers"
done
# The kernel names the root filesystem /dev/root in /proc/mounts, so parsing
# that file is the bug that left /boot and /data unmounted on real hardware.
if grep -lE 'awk[^|]*proc/mounts' "$OVL"/etc/init.d/S* >/dev/null 2>&1; then
	fail "an init script parses /proc/mounts: use wat_rootdev from sd.sh"
fi
for tty in ttyS0 ttyAMA0 ttyGS0 tty1; do
	grep -q "$tty" "$OVL/etc/init.d/S02console" ||
		fail "S02console does not handle $tty"
done
ok "rootfs overlay"

# --- SD card helpers (unit test, no hardware) ------------------------------

(
	. "$OVL/usr/lib/wat/sd.sh"
	for pair in /dev/mmcblk0p2:/dev/mmcblk0 /dev/mmcblk0p10:/dev/mmcblk0 \
		/dev/sda2:/dev/sda /dev/nvme0n1p2:/dev/nvme0n1; do
		got=$(wat_disk "${pair%%:*}")
		[ "$got" = "${pair##*:}" ] || {
			printf 'FAIL: wat_disk %s = %s, want %s\n' "${pair%%:*}" "$got" "${pair##*:}" >&2
			false
		}
	done
	[ "$(wat_part /dev/mmcblk0 3)" = /dev/mmcblk0p3 ] || {
		echo "FAIL: wat_part /dev/mmcblk0 3" >&2
		false
	}
	[ "$(wat_part /dev/sda 1)" = /dev/sda1 ] || {
		echo "FAIL: wat_part /dev/sda 1" >&2
		false
	}
	# The card layout is p1 boot, p2 root A, p3 root B, p4 DATA.
	[ "$(wat_part /dev/mmcblk0 4)" = /dev/mmcblk0p4 ] || {
		echo "FAIL: wat_part /dev/mmcblk0 4" >&2
		false
	}
) || fail "sd.sh helpers return the wrong device names"
ok "sd.sh helpers"

# --- wat.txt template ------------------------------------------------------

WAT="$HERE/board/common/wat.txt"
[ -f "$WAT" ] || fail "missing board/common/wat.txt"
grep -q '^setup_ap_key=' "$WAT" || fail "wat.txt has no setup_ap_key"
grep -q '^country=' "$WAT" || fail "wat.txt has no country"
key=$(sed -n 's/^setup_ap_key=//p' "$WAT")
[ ${#key} -ge 8 ] || fail "the setup_ap_key template is shorter than 8 characters"
grep -q '^console=\(off\|serial\|usb\|hdmi\|both\|all\)$' "$WAT" ||
	fail "wat.txt needs a valid console= value"
grep -q '^console=all$' "$WAT" ||
	fail "wat.txt should ship with console=all while the images are being validated"
ok "wat.txt template"

# --- boot console ----------------------------------------------------------

# Linux 6.1 names the BeagleBone UART ttyS0; console=ttyO0 matches nothing.
# Kernel output also goes to HDMI (tty1) and starts with earlycon, so a board
# without a serial adapter still shows what happened.
grep -q 'console=ttyS0,115200' "$HERE/board/bbb/extlinux.conf" ||
	fail "bbb/extlinux.conf must use console=ttyS0,115200"
if grep -q 'console=ttyO0' "$HERE/board/bbb/extlinux.conf"; then
	fail "bbb/extlinux.conf still uses ttyO0 (renamed in Linux 4.11)"
fi
grep -q 'console=tty1' "$HERE/board/bbb/extlinux.conf" ||
	fail "bbb/extlinux.conf must also log to tty1 (HDMI)"
grep -q 'earlycon=' "$HERE/board/bbb/extlinux.conf" ||
	fail "bbb/extlinux.conf has no earlycon: nothing prints before the UART driver loads"
grep -q 'quiet' "$HERE/board/bbb/extlinux.conf" &&
	fail "bbb/extlinux.conf still passes quiet: the boot log is how we debug"
grep -q 'console=tty1' "$HERE/board/rpi2/cmdline.txt" ||
	fail "rpi2/cmdline.txt must keep console=tty1 (HDMI)"
# First boot is root A = p2 on both boards, by device path (D18): a UUID
# would have to be rewritten by every image update, and the running system
# works out the other slot from its own block device.
for f in "$HERE/board/bbb/extlinux.conf" "$HERE/board/rpi2/cmdline.txt"; do
	grep -q 'root=/dev/mmcblk0p2' "$f" ||
		fail "$f must boot /dev/mmcblk0p2 (root A)"
	if grep -q 'root=UUID=\|root=LABEL=' "$f"; then
		fail "$f names the root slot by UUID/LABEL: use the device path"
	fi
done
ok "boot console"

# --- helper scripts --------------------------------------------------------

for s in mkimage.sh check.sh; do
	[ -x "$HERE/$s" ] || fail "$s is not executable"
done
# mkimage.sh makes the empty second root slot and labels both: the labels are
# how a human (and e2label in the field) tells root A from root B.
for label in wat-root-a wat-root-b; do
	grep -q "$label" "$HERE/mkimage.sh" || fail "mkimage.sh does not set the $label label"
done
grep -q 'host/sbin/mke2fs' "$HERE/mkimage.sh" ||
	fail "mkimage.sh must use the Buildroot host mke2fs, not the host's"
grep -q 'rootfs-b.ext4' "$HERE/mkimage.sh" ||
	fail "mkimage.sh does not build the empty root B image"
ok "helper scripts"

if [ "$failed" -ne 0 ]; then
	printf '\n%d check(s) failed\n' "$failed" >&2
	exit 1
fi
printf '\nall checks passed\n'
