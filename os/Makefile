# Buildroot images for wa-transcriber.
#
# Buildroot itself is downloaded and pinned here (not a submodule, not
# vendored) so a clean checkout needs one command per board:
#
#	make -C os rpi2     -> os/output/rpi2/images/sdcard.img
#	make -C os bbb      -> os/output/bbb/images/sdcard.img
#
# The transcriber binary is cross-compiled by the Go toolchain first (see
# "prebuilt" below) and then only copied into the target filesystem; Go is
# never built by Buildroot on purpose (its version lags, and our build is a
# static, cgo-free binary).

BUILDROOT_VERSION ?= 2024.02.9
BUILDROOT_TARBALL := buildroot-$(BUILDROOT_VERSION).tar.gz
BUILDROOT_URL     ?= https://buildroot.org/downloads/$(BUILDROOT_TARBALL)
BUILDROOT_DIR     := buildroot-$(BUILDROOT_VERSION)
BR2_EXTERNAL      := $(CURDIR)
GO_DIST           := $(CURDIR)/../dist/wa-transcriber-linux-armv7

JOBS ?= $(shell nproc 2>/dev/null || echo 2)

# One output directory per board, so both can be built from the same tree.
OUT_rpi2 := $(CURDIR)/output/rpi2
OUT_bbb  := $(CURDIR)/output/bbb

.PHONY: all rpi2 bbb prebuilt buildroot check clean distclean download menuconfig-rpi2 menuconfig-bbb

all: check rpi2 bbb

# --- the binary ------------------------------------------------------------

# Refresh os/prebuilt/ from the Go cross-build. The package copies
# os/prebuilt/wa-transcriber-linux-armv7 into /usr/bin/wa-transcriber.
prebuilt: $(GO_DIST)
	@mkdir -p prebuilt
	cp $(GO_DIST) prebuilt/wa-transcriber-linux-armv7
	@ls -l prebuilt/wa-transcriber-linux-armv7

$(GO_DIST):
	$(MAKE) -C .. build-arm7

# --- Buildroot -------------------------------------------------------------

buildroot: $(BUILDROOT_DIR)/.stamp-extracted

$(BUILDROOT_TARBALL):
	curl -fL -o $@ $(BUILDROOT_URL)

$(BUILDROOT_DIR)/.stamp-extracted: $(BUILDROOT_TARBALL)
	tar xzf $(BUILDROOT_TARBALL)
	touch $@

# --- board images ----------------------------------------------------------

rpi2: check buildroot prebuilt
	$(MAKE) -C $(BUILDROOT_DIR) O=$(OUT_rpi2) BR2_EXTERNAL=$(BR2_EXTERNAL) wat_rpi2_defconfig
	$(MAKE) -C $(BUILDROOT_DIR) O=$(OUT_rpi2) BR2_EXTERNAL=$(BR2_EXTERNAL) -j$(JOBS)
	sh $(CURDIR)/mkimage.sh rpi2 $(OUT_rpi2)

bbb: check buildroot prebuilt
	$(MAKE) -C $(BUILDROOT_DIR) O=$(OUT_bbb) BR2_EXTERNAL=$(BR2_EXTERNAL) wat_bbb_defconfig
	$(MAKE) -C $(BUILDROOT_DIR) O=$(OUT_bbb) BR2_EXTERNAL=$(BR2_EXTERNAL) -j$(JOBS)
	sh $(CURDIR)/mkimage.sh bbb $(OUT_bbb)

# The SD card image is assembled by mkimage.sh from the files Buildroot
# produced in $(OUT)/images, so the partition layout stays in this tree
# instead of in a Buildroot post-image hook.

menuconfig-rpi2: buildroot
	$(MAKE) -C $(BUILDROOT_DIR) O=$(OUT_rpi2) BR2_EXTERNAL=$(BR2_EXTERNAL) menuconfig

menuconfig-bbb: buildroot
	$(MAKE) -C $(BUILDROOT_DIR) O=$(OUT_bbb) BR2_EXTERNAL=$(BR2_EXTERNAL) menuconfig

# --- checks ----------------------------------------------------------------

check:
	@sh $(CURDIR)/check.sh

clean:
	rm -rf $(OUT_rpi2) $(OUT_bbb)

distclean: clean
	rm -rf $(BUILDROOT_DIR) $(BUILDROOT_TARBALL) prebuilt
