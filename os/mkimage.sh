#!/bin/sh
#
# Assemble the SD card image with genimage from the Buildroot output.
#
#	sh mkimage.sh <board> <buildroot-output-dir>
#
# genimage was built by Buildroot as a host tool (BR2_PACKAGE_HOST_GENIMAGE),
# so it is already in <out>/host/bin and no extra dependency is needed. The
# empty second root slot (p3, root B) is made here with e2fsprogs, which
# Buildroot builds as host tools too (host-e2fsprogs, used by BR2_TARGET_
# ROOTFS_EXT_2 for the first slot).

set -eu

BOARD="$1"
OUT="$2"
HERE="$(cd "$(dirname "$0")" && pwd)"

# The card layout (D18): p1 boot, p2 root A (the system), p3 root B (the
# slot an image update writes), p4 DATA (made on the device at first boot).
ROOT_SLOT_SIZE=160M

IMAGES="$OUT/images"
GENIMAGE="$OUT/host/bin/genimage"
MKE2FS="$OUT/host/sbin/mke2fs"
E2LABEL="$OUT/host/sbin/e2label"
CFG="$HERE/board/$BOARD/genimage.cfg"

for tool in "$GENIMAGE" "$MKE2FS" "$E2LABEL"; do
	if [ ! -x "$tool" ]; then
		echo "mkimage: $tool not found (did the build finish?)" >&2
		exit 1
	fi
done
if [ ! -f "$CFG" ]; then
	echo "mkimage: $CFG not found" >&2
	exit 1
fi

# genimage copies --rootpath into the filesystems it builds; ours are already
# prepared in $IMAGES, so it only needs an empty directory.
ROOTPATH="$(mktemp -d)"
EMPTYDIR="$(mktemp -d)"
trap 'rm -rf "$ROOTPATH" "$EMPTYDIR"' EXIT
TMPPATH="$OUT/genimage.tmp"
rm -rf "$TMPPATH"

# genimage sizes a partition from its image file, and fails on an input file
# that does not exist, so p3 would take its 160M from the file it is given:
# leave p3 without a file next to p2 and the last partition takes the rest of
# the disk instead. Buildroot names the system's filesystem rootfs.ext2 or
# rootfs.ext4 (the "2" in the real name was fixed by a symlink); label it
# here, so the two slots can be told apart on a board or in a card reader.
rootfs=""
for f in "$IMAGES/rootfs.ext4" "$IMAGES/rootfs.ext2"; do
	if [ -f "$f" ]; then
		rootfs="$f"
		break
	fi
done
if [ -z "$rootfs" ]; then
	echo "mkimage: no rootfs.ext4/rootfs.ext2 in $IMAGES (did the build finish?)" >&2
	exit 1
fi
"$E2LABEL" "$rootfs" wat-root-a

# root B: an empty ext4, never written to before the first image update. It
# only has to be a valid filesystem with the right label and the same geometry
# as root A (1024-byte blocks, which is what Buildroot's mke2fs.conf picks for
# a 160M filesystem); -d keeps the root directory fully allocated instead of
# leaving uninitialised inode tables, which an fsck on the board would print as
# errors after a power cut.
rm -f "$IMAGES/rootfs-b.ext4"
"$MKE2FS" -q -F -t ext4 -L wat-root-b -d "$EMPTYDIR" -E root_owner=0:0 \
	"$IMAGES/rootfs-b.ext4" "$ROOT_SLOT_SIZE"

"$GENIMAGE" \
	--rootpath "$ROOTPATH" \
	--tmppath "$TMPPATH" \
	--inputpath "$IMAGES" \
	--outputpath "$IMAGES" \
	--config "$CFG"

ls -l "$IMAGES/sdcard.img"
