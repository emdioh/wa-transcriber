# Buildroot images for wa-transcriber.
#
# Buildroot itself is downloaded and pinned here (not a submodule, not
# vendored) so a clean checkout needs one command per board:
#
#	make -C os rpi1     -> os/_output/rpi1/images/sdcard.img
#	make -C os rpi2     -> os/_output/rpi2/images/sdcard.img
#	make -C os bbb      -> os/_output/bbb/images/sdcard.img
#	make -C os cubie    -> os/_output/cubie/images/sdcard.img
#
# The transcriber binary is cross-compiled by the Go toolchain first (see
# "prebuilt" below) and then only copied into the target filesystem; Go is
# never built by Buildroot on purpose (its version lags, and our build is a
# static, cgo-free binary).

BUILDROOT_VERSION ?= 2024.02.9

# Everything generated lives under _-prefixed directories: the Go toolchain
# ignores those, so "go build ./..." never walks into a Buildroot tree.
BUILDROOT_ROOT    := $(CURDIR)/_buildroot
BUILDROOT_DIR     := $(BUILDROOT_ROOT)/buildroot-$(BUILDROOT_VERSION)
BUILDROOT_TARBALL := $(BUILDROOT_ROOT)/buildroot-$(BUILDROOT_VERSION).tar.gz
BUILDROOT_URL     ?= https://buildroot.org/downloads/buildroot-$(BUILDROOT_VERSION).tar.gz
BR2_EXTERNAL      := $(CURDIR)
GO_DIST_armv7     := $(CURDIR)/../dist/wa-transcriber-linux-armv7
GO_DIST_armv6     := $(CURDIR)/../dist/wa-transcriber-linux-armv6

JOBS ?= $(shell nproc 2>/dev/null || echo 2)

# One output directory per board, so all can be built from the same tree.
OUT_rpi1  := $(CURDIR)/_output/rpi1
OUT_rpi2  := $(CURDIR)/_output/rpi2
OUT_bbb   := $(CURDIR)/_output/bbb
OUT_cubie := $(CURDIR)/_output/cubie

.PHONY: all rpi1 rpi2 bbb cubie prebuilt buildroot check clean distclean download menuconfig-rpi1 menuconfig-rpi2 menuconfig-bbb menuconfig-cubie

all: check rpi1 rpi2 bbb cubie

# --- the binary ------------------------------------------------------------

# Refresh os/prebuilt/ from the Go cross-build. The package copies
# os/prebuilt/wa-transcriber-linux-armv7 into /usr/bin/wa-transcriber.
prebuilt: $(GO_DIST_armv7) $(GO_DIST_armv6)
	@mkdir -p prebuilt
	cp $(GO_DIST_armv7) prebuilt/wa-transcriber-linux-armv7
	cp $(GO_DIST_armv6) prebuilt/wa-transcriber-linux-armv6
	@ls -l prebuilt/wa-transcriber-linux-arm*

# Buildroot sees a "local" package source as immutable: without this the
# image keeps whatever binary the first build copied (the board once booted
# a binary three commits old). The board targets run wa-transcriber-dirclean
# before building for that reason.

# Rebuild the binary whenever the Go sources change: os/prebuilt is a plain
# copy, and a stale copy here put a three-commit-old binary on a board. The
# update public key counts as a source: it reaches the binary through the
# Makefile's -X, and an image built before the key was committed has
# self-updates compiled out.
GO_SOURCES := $(shell find $(CURDIR)/.. -name '*.go' -not -path '*/_output/*' \
	-not -path '*/dist/*' -not -path '*/data/*' 2>/dev/null) \
	$(CURDIR)/../go.mod $(CURDIR)/../go.sum $(CURDIR)/../release/update-pubkey.txt

$(GO_DIST_armv7): $(GO_SOURCES)
	$(MAKE) -C .. build-arm7

$(GO_DIST_armv6): $(GO_SOURCES)
	$(MAKE) -C .. build-arm6

# --- Buildroot -------------------------------------------------------------

buildroot: $(BUILDROOT_DIR)/.stamp-extracted

$(BUILDROOT_TARBALL):
	curl -fL -o $@ $(BUILDROOT_URL)

$(BUILDROOT_DIR)/.stamp-extracted: $(BUILDROOT_TARBALL)
	@mkdir -p $(BUILDROOT_ROOT)
	tar xzf $(BUILDROOT_TARBALL) -C $(BUILDROOT_ROOT)
	touch $@

# --- board images ----------------------------------------------------------

rpi1: check buildroot prebuilt
	$(MAKE) -C $(BUILDROOT_DIR) O=$(OUT_rpi1) BR2_EXTERNAL=$(BR2_EXTERNAL) wat_rpi1_defconfig
	$(MAKE) -C $(BUILDROOT_DIR) O=$(OUT_rpi1) BR2_EXTERNAL=$(BR2_EXTERNAL) wa-transcriber-dirclean rpi-firmware-dirclean
	$(MAKE) -C $(BUILDROOT_DIR) O=$(OUT_rpi1) BR2_EXTERNAL=$(BR2_EXTERNAL) -j$(JOBS)
	sh $(CURDIR)/mkimage.sh rpi1 $(OUT_rpi1)

rpi2: check buildroot prebuilt
	$(MAKE) -C $(BUILDROOT_DIR) O=$(OUT_rpi2) BR2_EXTERNAL=$(BR2_EXTERNAL) wat_rpi2_defconfig
	$(MAKE) -C $(BUILDROOT_DIR) O=$(OUT_rpi2) BR2_EXTERNAL=$(BR2_EXTERNAL) wa-transcriber-dirclean rpi-firmware-dirclean
	$(MAKE) -C $(BUILDROOT_DIR) O=$(OUT_rpi2) BR2_EXTERNAL=$(BR2_EXTERNAL) -j$(JOBS)
	sh $(CURDIR)/mkimage.sh rpi2 $(OUT_rpi2)

bbb: check buildroot prebuilt
	$(MAKE) -C $(BUILDROOT_DIR) O=$(OUT_bbb) BR2_EXTERNAL=$(BR2_EXTERNAL) wat_bbb_defconfig
	$(MAKE) -C $(BUILDROOT_DIR) O=$(OUT_bbb) BR2_EXTERNAL=$(BR2_EXTERNAL) wa-transcriber-dirclean
	$(MAKE) -C $(BUILDROOT_DIR) O=$(OUT_bbb) BR2_EXTERNAL=$(BR2_EXTERNAL) -j$(JOBS)
	sh $(CURDIR)/mkimage.sh bbb $(OUT_bbb)

cubie: check buildroot prebuilt
	$(MAKE) -C $(BUILDROOT_DIR) O=$(OUT_cubie) BR2_EXTERNAL=$(BR2_EXTERNAL) wat_cubie_defconfig
	$(MAKE) -C $(BUILDROOT_DIR) O=$(OUT_cubie) BR2_EXTERNAL=$(BR2_EXTERNAL) wa-transcriber-dirclean
	$(MAKE) -C $(BUILDROOT_DIR) O=$(OUT_cubie) BR2_EXTERNAL=$(BR2_EXTERNAL) -j$(JOBS)
	sh $(CURDIR)/mkimage.sh cubie $(OUT_cubie)

# The SD card image is assembled by mkimage.sh from the files Buildroot
# produced in $(OUT)/images, so the partition layout stays in this tree
# instead of in a Buildroot post-image hook.

menuconfig-rpi1: buildroot
	$(MAKE) -C $(BUILDROOT_DIR) O=$(OUT_rpi1) BR2_EXTERNAL=$(BR2_EXTERNAL) menuconfig

menuconfig-rpi2: buildroot
	$(MAKE) -C $(BUILDROOT_DIR) O=$(OUT_rpi2) BR2_EXTERNAL=$(BR2_EXTERNAL) menuconfig

menuconfig-bbb: buildroot
	$(MAKE) -C $(BUILDROOT_DIR) O=$(OUT_bbb) BR2_EXTERNAL=$(BR2_EXTERNAL) menuconfig

menuconfig-cubie: buildroot
	$(MAKE) -C $(BUILDROOT_DIR) O=$(OUT_cubie) BR2_EXTERNAL=$(BR2_EXTERNAL) menuconfig

# --- checks ----------------------------------------------------------------

check:
	@sh $(CURDIR)/check.sh

clean:
	rm -rf $(OUT_rpi2) $(OUT_bbb) $(OUT_rpi1) $(OUT_cubie)

distclean: clean
	rm -rf $(BUILDROOT_ROOT) $(OUT_rpi2) $(OUT_bbb) $(OUT_rpi1) $(OUT_cubie) prebuilt
