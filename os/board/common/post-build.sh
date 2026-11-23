#!/bin/sh
#
# Common post-build step for both boards: install wat.txt on the boot
# partition (it is put there by genimage via the board config), make sure
# the release-specific bits are in place and drop build-time leftovers.
#
# $1 is the target directory.

set -eu

TARGET_DIR="$1"

# The data directory must exist even when the DATA partition is missing, so
# the process can start and show the problem in the web UI.
mkdir -p "$TARGET_DIR/data"

# Mountpoint for the boot partition: the root filesystem is read-only, so
# S00boot cannot create it, and wat.txt is read through it.
mkdir -p "$TARGET_DIR/boot"

# Read-only root: no network manager from the distro and, above all, no
# distro dnsmasq: the setup AP starts its own (port 53/67 would clash).
rm -f "$TARGET_DIR/etc/init.d/S40network" "$TARGET_DIR/etc/init.d/S80dnsmasq" 2>/dev/null || true

# wpa_supplicant is driven by the binary, never by a distro init script.
rm -f "$TARGET_DIR/etc/init.d/S45wpa_supplicant" "$TARGET_DIR/etc/init.d/S95wpa_supplicant" 2>/dev/null || true

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
