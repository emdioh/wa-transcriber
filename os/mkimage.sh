#!/bin/sh
#
# Assemble the SD card image with genimage from the Buildroot output.
#
#	sh mkimage.sh <board> <buildroot-output-dir>
#
# genimage was built by Buildroot as a host tool (BR2_PACKAGE_HOST_GENIMAGE),
# so it is already in <out>/host/bin and no extra dependency is needed.

set -eu

BOARD="$1"
OUT="$2"
HERE="$(cd "$(dirname "$0")" && pwd)"

IMAGES="$OUT/images"
GENIMAGE="$OUT/host/bin/genimage"
CFG="$HERE/board/$BOARD/genimage.cfg"

if [ ! -x "$GENIMAGE" ]; then
	echo "mkimage: $GENIMAGE not found (did the build finish?)" >&2
	exit 1
fi
if [ ! -f "$CFG" ]; then
	echo "mkimage: $CFG not found" >&2
	exit 1
fi

# genimage copies --rootpath into the filesystems it builds; ours are already
# prepared in $IMAGES, so it only needs an empty directory.
ROOTPATH="$(mktemp -d)"
trap 'rm -rf "$ROOTPATH"' EXIT
TMPPATH="$OUT/genimage.tmp"
rm -rf "$TMPPATH"

"$GENIMAGE" \
	--rootpath "$ROOTPATH" \
	--tmppath "$TMPPATH" \
	--inputpath "$IMAGES" \
	--outputpath "$IMAGES" \
	--config "$CFG"

ls -l "$IMAGES/sdcard.img"
