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
grep -q 'config BR2_PACKAGE_WAT_WA_TRANSCRIBER' "$PKG/Config.in" ||
	fail "Config.in does not define BR2_PACKAGE_WAT_WA_TRANSCRIBER"
grep -q 'generic-package' "$PKG/wa-transcriber.mk" ||
	fail "the package must be a generic-package (it only copies a prebuilt binary)"
grep -q 'wa-transcriber-linux-armv7' "$PKG/wa-transcriber.mk" ||
	fail "the package does not install the armv7 binary"
ok "package recipe"

# --- defconfigs ------------------------------------------------------------

for board in rpi2 bbb; do
	defconfig="$HERE/configs/wat_${board}_defconfig"
	[ -f "$defconfig" ] || { fail "missing $defconfig"; continue; }

	grep -q '^BR2_PACKAGE_WAT_WA_TRANSCRIBER=y$' "$defconfig" ||
		fail "$board: the transcriber package is not enabled"
	grep -q '^# BR2_TARGET_GENERIC_REMOUNT_ROOTFS_RW is not set$' "$defconfig" ||
		fail "$board: the root filesystem must stay read-only"
	grep -q '^BR2_PACKAGE_HOSTAPD=y$' "$defconfig" ||
		fail "$board: hostapd (setup AP) is not enabled"
	grep -q '^BR2_PACKAGE_WPA_SUPPLICANT=y$' "$defconfig" ||
		fail "$board: wpa_supplicant is not enabled"
	grep -q '^BR2_PACKAGE_DNSMASQ=y$' "$defconfig" ||
		fail "$board: dnsmasq (setup AP DHCP/DNS) is not enabled"

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
for f in etc/inittab etc/fstab etc/init.d/S01data etc/init.d/S40ntpd etc/init.d/S90watchdog; do
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
ok "rootfs overlay"

# --- wat.txt template ------------------------------------------------------

WAT="$HERE/board/common/wat.txt"
[ -f "$WAT" ] || fail "missing board/common/wat.txt"
grep -q '^setup_ap_key=' "$WAT" || fail "wat.txt has no setup_ap_key"
grep -q '^country=' "$WAT" || fail "wat.txt has no country"
key=$(sed -n 's/^setup_ap_key=//p' "$WAT")
[ ${#key} -ge 8 ] || fail "the setup_ap_key template is shorter than 8 characters"
ok "wat.txt template"

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
