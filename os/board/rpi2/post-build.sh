#!/bin/sh
#
# Raspberry Pi 2 post-build: the RPi firmware files, the kernel and the
# device tree are copied into the images directory by Buildroot; here we add
# the two plain-text files that go on the boot partition and hand over to the
# common script.

set -eu

BOARD_DIR="$(dirname "$0")"
COMMON_DIR="${BR2_EXTERNAL_WAT_PATH}/board/common"

# cmdline.txt and config.txt are picked up from BR2_EXTERNAL by genimage; copy
# the current ones into the images directory so genimage finds them there.
cp "$BOARD_DIR/cmdline.txt" "$BINARIES_DIR/cmdline.txt"
cp "$BOARD_DIR/config.txt" "$BINARIES_DIR/config.txt"
# wat.txt: first-boot settings, editable from any computer (FAT partition).
cp "$COMMON_DIR/wat.txt" "$BINARIES_DIR/wat.txt"

exec "$COMMON_DIR/post-build.sh" "$@"
