#!/bin/sh
#
# Structural checks for the BR2_EXTERNAL tree. Run by "make -C os check" (and
# therefore before any image build); it does not need Buildroot or hardware.
#
# It catches the mistakes that otherwise only show up an hour into a build
# or, worse, at boot: a missing file referenced by a defconfig, a package
# recipe without its name prefix, forbidden legacy packages sneaking back in,
# or a genimage config naming a file the board never produces.

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
grep -q 'wa-transcriber-linux-$(WA_TRANSCRIBER_ARCH)' "$PKG/wa-transcriber.mk" ||
	fail "the package names a fixed architecture instead of \$(WA_TRANSCRIBER_ARCH)"
grep -q 'default "armv7"' "$PKG/Config.in" ||
	fail "BR2_PACKAGE_WA_TRANSCRIBER_ARCH does not default to armv7"
ok "package recipe"

# --- defconfigs ------------------------------------------------------------

FRAG="$HERE/board/common/linux.fragment"

for board in rpi1 rpi2 bbb cubie; do
	defconfig="$HERE/configs/wat_${board}_defconfig"
	[ -f "$defconfig" ] || { fail "missing $defconfig"; continue; }

	# Required barebones appliance settings (D19).
	for req in \
		'^BR2_INIT_NONE=y$' \
		'^BR2_SYSTEM_BIN_SH_NONE=y$' \
		'^BR2_ROOTFS_DEVICE_CREATION_DYNAMIC_DEVTMPFS=y$' \
		'^# BR2_PACKAGE_BUSYBOX is not set$' \
		'^# BR2_TARGET_GENERIC_REMOUNT_ROOTFS_RW is not set$' \
		'^BR2_PACKAGE_WA_TRANSCRIBER=y$' \
		'^BR2_PACKAGE_E2FSPROGS=y$' \
		'^BR2_PACKAGE_WPA_SUPPLICANT=y$' \
		'^BR2_PACKAGE_WPA_SUPPLICANT_NL80211=y$' \
		'^BR2_PACKAGE_HOSTAPD=y$' \
		'^BR2_PACKAGE_HOSTAPD_DRIVER_NL80211=y$' \
		'^BR2_PACKAGE_WIRELESS_REGDB=y$' \
		'^BR2_PACKAGE_CA_CERTIFICATES=y$'; do
		grep -q "$req" "$defconfig" ||
			fail "$board: defconfig missing required setting: $req"
	done

	# Forbidden legacy packages/options that were stripped from the rootfs.
	for forbid in \
		'^BR2_PACKAGE_BUSYBOX=y$' \
		'^BR2_INIT_BUSYBOX=y$' \
		'^BR2_ROOTFS_DEVICE_CREATION_DYNAMIC_MDEV=y$' \
		'^BR2_PACKAGE_KMOD=y$' \
		'^BR2_PACKAGE_UTIL_LINUX=y$' \
		'^BR2_PACKAGE_DNSMASQ=y$' \
		'^BR2_PACKAGE_IW=y$' \
		'^BR2_PACKAGE_WPA_SUPPLICANT_CLI=y$' \
		'^BR2_PACKAGE_BUSYBOX_CONFIG_FRAGMENT_FILES=' \
		'^BR2_ROOTFS_OVERLAY='; do
		if grep -q "$forbid" "$defconfig"; then
			fail "$board: defconfig still enables forbidden legacy option: $forbid"
		fi
	done

	# The binary has to match the CPU: an armv7 Go binary on an ARMv6 board
	# (Raspberry Pi 1/Zero) loads and dies with no output, and vice versa.
	want_arch=armv7
	[ "$board" = rpi1 ] && want_arch=armv6
	if grep -q '^BR2_PACKAGE_WA_TRANSCRIBER_ARCH=' "$defconfig"; then
		grep -q "^BR2_PACKAGE_WA_TRANSCRIBER_ARCH=\"$want_arch\"\$" "$defconfig" ||
			fail "$board: BR2_PACKAGE_WA_TRANSCRIBER_ARCH must be \"$want_arch\""
	elif [ "$want_arch" != armv7 ]; then
		fail "$board: needs BR2_PACKAGE_WA_TRANSCRIBER_ARCH=\"$want_arch\" (default is armv7)"
	fi

	# The WiFi drivers in the kernel fragment need their blobs in the rootfs.
	# A missing one is not a build error, it is a dead dongle on the board.
	if grep -q '^CONFIG_MT76x2U=[ym]$' "$FRAG"; then
		grep -q '^BR2_PACKAGE_LINUX_FIRMWARE_MEDIATEK_MT76X2E=y$' "$defconfig" ||
			fail "$board: mt76x2u needs linux-firmware MEDIATEK_MT76X2E (mt7662*.bin)"
	fi
	if grep -q '^CONFIG_RTL8XXXU=[ym]$' "$FRAG"; then
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
	for ref in $(sed -n 's/.*BR2_EXTERNAL_WAT_PATH)\/\([^"]*\)".*/\1/p' "$defconfig"); do
		[ -e "$HERE/$ref" ] || fail "$board: defconfig references missing path $ref"
	done
	ok "defconfig $board"
done

# --- board files -----------------------------------------------------------

for board in rpi1 rpi2 bbb cubie; do
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
	# The boot files name the first slot by device path, and no MBR partition
	# number after p3 may exist in the image: DATA is created on the board.
	# Raw bootloader slots with in-partition-table = "no" (such as sunxi SPL
	# at 8K on the Cubieboard) do not take an MBR entry.
	mbr_parts=$(awk '
		/^[[:space:]]*partition[[:space:]]+/ { in_part = 1; in_tbl = 1 }
		in_part && /in-partition-table[[:space:]]*=[[:space:]]*"no"/ { in_tbl = 0 }
		in_part && /^[[:space:]]*}/ { if (in_tbl) count++; in_part = 0 }
		END { print count + 0 }
	' "$HERE/board/$board/genimage.cfg")
	[ "$mbr_parts" -eq 3 ] ||
		fail "board/$board/genimage.cfg must define exactly three MBR partitions (boot, root A, root B), got $mbr_parts"
	if [ "$board" = cubie ]; then
		awk '
			/^[[:space:]]*partition[[:space:]]+u-boot/ { in_u = 1 }
			in_u && /in-partition-table[[:space:]]*=[[:space:]]*"no"/ { no_tbl = 1 }
			in_u && /image[[:space:]]*=[[:space:]]*"u-boot-sunxi-with-spl\.bin"/ { has_img = 1 }
			in_u && /offset[[:space:]]*=[[:space:]]*8K/ { has_off = 1 }
			in_u && /^[[:space:]]*}/ { in_u = 0 }
			END { exit !(no_tbl && has_img && has_off) }
		' "$HERE/board/cubie/genimage.cfg" ||
			fail "board/cubie/genimage.cfg must place u-boot-sunxi-with-spl.bin with in-partition-table = \"no\" and offset = 8K"
	fi
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

for board in bbb:beagle:/boot/extlinux/extlinux.conf cubie:cubie:/boot/extlinux/extlinux.conf rpi1:rpi1:/boot/cmdline.txt rpi2:rpi2:/boot/cmdline.txt; do
	dir=${board%%:*}; rest=${board#*:}; name=${rest%%:*}; boot=${rest#*:}
	grep -q '"boot_file": "'"$boot"'"' "$HERE/board/$dir/post-build.sh" ||
		fail "board/$dir/post-build.sh does not write etc/wat/image.json with boot_file $boot"
	grep -q '"board": "'"$name"'"' "$HERE/board/$dir/post-build.sh" ||
		fail "board/$dir/post-build.sh does not name the manifest board $name"
done

# --- barebones appliance, kernel & post-build ------------------------------

OVL="$HERE/board/common/rootfs-overlay"
[ ! -e "$HERE/board/common/busybox.fragment" ] ||
	fail "legacy board/common/busybox.fragment must be removed"
[ ! -e "$OVL/etc/inittab" ] ||
	fail "legacy rootfs-overlay/etc/inittab must be removed"
for s in "$OVL"/etc/init.d/S*; do
	if [ -e "$s" ]; then
		fail "legacy init script must be removed: $s"
	fi
done

# All WiFi and watchdog drivers must be built-in (=y) with CONFIG_FW_LOADER=y
# so the kernel requires neither kmod/modprobe nor mdev.
for cfg in \
	CONFIG_FW_LOADER=y \
	CONFIG_CFG80211=y \
	CONFIG_MAC80211=y \
	CONFIG_RFKILL=y \
	CONFIG_RT2X00=y \
	CONFIG_RT2800USB=y \
	CONFIG_RT2800_LIB=y \
	CONFIG_MT76_CORE=y \
	CONFIG_MT76_USB=y \
	CONFIG_MT76x2_COMMON=y \
	CONFIG_MT76x2U=y \
	CONFIG_RTL8XXXU=y \
	CONFIG_BCM2835_WDT=y \
	CONFIG_OMAP_WDT=y \
	CONFIG_SUNXI_WDT=y; do
	grep -q "^${cfg}\$" "$FRAG" ||
		fail "board/common/linux.fragment must enable $cfg"
done
if grep -q '=m$' "$FRAG"; then
	fail "board/common/linux.fragment must not build drivers as modules (=m)"
fi

# Common post-build.sh prepares the read-only rootfs mountpoints, resolv.conf
# symlink, strips /etc/init.d, and links MediaTek firmware blobs.
COMMON_PB="$HERE/board/common/post-build.sh"
[ -x "$COMMON_PB" ] || fail "board/common/post-build.sh is not executable"
grep -q 'mkdir -p "\$TARGET_DIR/boot"' "$COMMON_PB" ||
	fail "post-build.sh does not create the /boot mountpoint"
grep -q 'mkdir -p "\$TARGET_DIR/data"' "$COMMON_PB" ||
	fail "post-build.sh does not create the /data mountpoint"
grep -q 'ln -sf /tmp/resolv.conf "\$TARGET_DIR/etc/resolv.conf"' "$COMMON_PB" ||
	fail "post-build.sh does not symlink /etc/resolv.conf to /tmp/resolv.conf"
grep -q 'rm -rf "\$TARGET_DIR/etc/init.d"' "$COMMON_PB" ||
	fail "post-build.sh does not remove /etc/init.d"
grep -qF 'ln -sf "mediatek/$fw" "$TARGET_DIR/lib/firmware/$fw"' "$COMMON_PB" ||
	fail "post-build.sh does not link mediatek/mt7662*.bin where the kernel looks"

# Init mode (D19): every board boots the transcriber as PID 1, and initmode.go
# carries all board defaults directly.
for f in board/bbb/extlinux.conf board/cubie/extlinux.conf board/rpi1/cmdline.txt board/rpi2/cmdline.txt; do
	grep -q 'init=/usr/bin/wa-transcriber' "$HERE/$f" ||
		fail "$f does not start the transcriber as init (D19)"
done
INITMODE="$HERE/../cmd/wa-transcriber/initmode.go"
for setting in 'data=boot.DataDir' 'listen=":80"' 'network="managed"' \
	'watchdog="/dev/watchdog"' 'update-dir="/data/bin"' \
	'bootstrap="/usr/bin/wa-transcriber"' 'image-update="/data"'; do
	name=${setting%%=*}
	value=${setting#*=}
	grep -qE "^[[:space:]]*\"$name\":[[:space:]]*$value" "$INITMODE" ||
		fail "initmode.go lacks the init default \"$name\": $value"
done

# Pure-Go partitioning and filesystem handling (no sfdisk, no partx, no shell).
BOOTPKG="$HERE/../internal/boot"
grep -q 'part\.AppendMBRPartition' "$BOOTPKG/disk.go" ||
	fail "internal/boot/disk.go does not append the DATA partition with part.AppendMBRPartition"
grep -q 'part\.AddKernelPartition' "$BOOTPKG/disk.go" ||
	fail "internal/boot/disk.go does not register the new partition with part.AddKernelPartition"
grep -q '"mkfs.ext4", "-q", "-L", DataLabel' "$BOOTPKG/disk.go" ||
	fail "internal/boot/disk.go does not format a fresh DATA partition with its label"
grep -q '"e2fsck", "-p"' "$BOOTPKG/disk.go" ||
	fail "internal/boot/disk.go does not check the data filesystem once after a failed mount"

# Buildroot treats the local package source as immutable, so the board
# targets must dirclean it or the image keeps the first binary it ever had.
grep -q "wa-transcriber-dirclean" "$HERE/Makefile" ||
	fail "os/Makefile must dirclean the transcriber package before building"
ok "barebones appliance rootfs"

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

# Linux 6.1 names the BeagleBone and Cubieboard UART ttyS0; console=ttyO0
# matches nothing on BBB. Kernel output also goes to HDMI (tty1) and starts
# with earlycon, so a board without a serial adapter still shows what happened.
for board in bbb cubie; do
	cfg="$HERE/board/$board/extlinux.conf"
	grep -q 'console=ttyS0,115200' "$cfg" ||
		fail "$board/extlinux.conf must use console=ttyS0,115200"
	grep -q 'console=tty1' "$cfg" ||
		fail "$board/extlinux.conf must also log to tty1 (HDMI)"
	grep -q 'earlycon=' "$cfg" ||
		fail "$board/extlinux.conf has no earlycon: nothing prints before the UART driver loads"
	grep -q 'quiet' "$cfg" &&
		fail "$board/extlinux.conf still passes quiet: the boot log is how we debug"
done
if grep -q 'console=ttyO0' "$HERE/board/bbb/extlinux.conf"; then
	fail "bbb/extlinux.conf still uses ttyO0 (renamed in Linux 4.11)"
fi
for board in rpi1 rpi2; do
	# Raspberry Pi firmware reads only the first line of cmdline.txt.
	[ "$(wc -l < "$HERE/board/$board/cmdline.txt")" -eq 1 ] ||
		fail "$board/cmdline.txt must be a single line"
	grep -q 'console=tty1' "$HERE/board/$board/cmdline.txt" ||
		fail "$board/cmdline.txt must keep console=tty1 (HDMI)"
	# gpu_mem=16 makes bootcode.bin load start_cd.elf + fixup_cd.dat instead of
	# start.elf; missing them stalls the Pi with 3 LED flashes and no rainbow.
	if grep -q '^gpu_mem=16$' "$HERE/board/$board/config.txt"; then
		grep -q '^BR2_PACKAGE_RPI_FIRMWARE_VARIANT_PI_CD=y$' "$HERE/configs/wat_${board}_defconfig" ||
			fail "$board: gpu_mem=16 requires BR2_PACKAGE_RPI_FIRMWARE_VARIANT_PI_CD=y in wat_${board}_defconfig"
		for fw in start_cd.elf fixup_cd.dat; do
			grep -q "\"rpi-firmware/$fw\"" "$HERE/board/$board/genimage.cfg" ||
				fail "$board: gpu_mem=16 requires rpi-firmware/$fw in genimage.cfg"
		done
	fi
done
# First boot is root A = p2 on all boards, by device path (D18): a UUID
# would have to be rewritten by every image update, and the running system
# works out the other slot from its own block device.
for f in "$HERE/board/bbb/extlinux.conf" "$HERE/board/cubie/extlinux.conf" "$HERE/board/rpi1/cmdline.txt" "$HERE/board/rpi2/cmdline.txt"; do
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
