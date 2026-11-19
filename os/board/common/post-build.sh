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

# Read-only root: no default password-based getty, no network manager from
# the distro, nothing else claims the interfaces.
rm -f "$TARGET_DIR/etc/init.d/S40network" 2>/dev/null || true

# Keep the image small: no docs, no locales.
rm -rf "$TARGET_DIR/usr/share/man" "$TARGET_DIR/usr/share/doc" \
       "$TARGET_DIR/usr/share/info" 2>/dev/null || true
