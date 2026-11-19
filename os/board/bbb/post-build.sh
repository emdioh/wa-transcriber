#!/bin/sh
#
# BeagleBone Black post-build: uEnv.txt/extlinux for the U-Boot boot flow and
# the plain-text files that live on the boot partition.

set -eu

BOARD_DIR="$(dirname "$0")"
COMMON_DIR="${BR2_EXTERNAL_WAT_PATH}/board/common"

install -m 0644 -D "$BOARD_DIR/uEnv.txt" "$BINARIES_DIR/uEnv.txt"
install -m 0644 -D "$BOARD_DIR/extlinux.conf" "$BINARIES_DIR/extlinux/extlinux.conf"
cp "$COMMON_DIR/wat.txt" "$BINARIES_DIR/wat.txt"

exec "$COMMON_DIR/post-build.sh" "$@"
