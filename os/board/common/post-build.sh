#!/bin/sh
#
# Common post-build step for all boards: ensure all mountpoints exist on the
# read-only rootfs, point /etc/resolv.conf at the /tmp tmpfs, remove any
# distro init scripts or docs, and link WiFi firmware where Linux 6.1 looks.
#
# $1 is the target directory.

set -eu

TARGET_DIR="$1"

# All mountpoints used by internal/boot must exist on the read-only rootfs.
mkdir -p "$TARGET_DIR/data"
mkdir -p "$TARGET_DIR/boot"
mkdir -p "$TARGET_DIR/proc" "$TARGET_DIR/sys" "$TARGET_DIR/dev" \
         "$TARGET_DIR/tmp" "$TARGET_DIR/run" \
         "$TARGET_DIR/var/log" "$TARGET_DIR/var/lib" "$TARGET_DIR/var/lib/misc"

# The root filesystem is read-only; resolv.conf lives on the /tmp tmpfs so
# the pure-Go DHCP client can update nameservers at runtime.
mkdir -p "$TARGET_DIR/etc"
ln -sf /tmp/resolv.conf "$TARGET_DIR/etc/resolv.conf"

# Barebones appliance (D19): wa-transcriber is PID 1, no init scripts exist.
rm -rf "$TARGET_DIR/etc/init.d"

# Keep the image small: no docs, no locales.
rm -rf "$TARGET_DIR/usr/share/man" "$TARGET_DIR/usr/share/doc" \
       "$TARGET_DIR/usr/share/info" 2>/dev/null || true

# linux-firmware ships the MediaTek blobs under mediatek/, but Linux 6.1 asks
# for them at the top of /lib/firmware (MT7662_FIRMWARE in mt76x2.h): link them
# where mt76x2u looks, or the MT7612U dongle dies on "failed to load firmware".
for fw in mt7662.bin mt7662_rom_patch.bin; do
	if [ ! -f "$TARGET_DIR/lib/firmware/mediatek/$fw" ]; then
		echo "post-build: missing lib/firmware/mediatek/$fw (linux-firmware MT76x2E)" >&2
		exit 1
	fi
	ln -sf "mediatek/$fw" "$TARGET_DIR/lib/firmware/$fw"
done
