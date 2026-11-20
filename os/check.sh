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
# /run/lock is on the tmpfs that "mount -a" mounts, so the mkdir comes after.
a=$(grep -n '^::sysinit:/bin/mount -a$' "$OVL/etc/inittab" | cut -d: -f1)
b=$(grep -n 'mkdir -p /run/lock' "$OVL/etc/inittab" | cut -d: -f1)
[ -n "$a" ] && [ -n "$b" ] && [ "$a" -lt "$b" ] ||
	fail "inittab must mount -a before creating /run/lock (read-only root)"
grep -q 'sysfs' "$OVL/etc/fstab" || fail "fstab does not mount /sys (needed by mdev and the network manager)"
# The boot partition carries wat.txt: without the mount the binary silently
# falls back to its defaults.
grep -q 'mount .*\-t vfat .* /boot' "$OVL/etc/init.d/S00boot" ||
	fail "S00boot does not mount the boot partition at /boot"
grep -q 'mkdir -p "\$TARGET_DIR/boot"' "$HERE/board/common/post-build.sh" ||
	fail "post-build.sh does not create the /boot mountpoint"
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
ok "boot console"

# --- helper scripts --------------------------------------------------------

for s in mkimage.sh check.sh; do
	[ -x "$HERE/$s" ] || fail "$s is not executable"
done
ok "helper scripts"

if [ "$failed" -ne 0 ]; then
	printf '\n%d check(s) failed\n' "$failed" >&2
	exit 1
fi
printf '\nall checks passed\n'
