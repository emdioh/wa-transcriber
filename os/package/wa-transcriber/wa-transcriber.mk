################################################################################
#
# wa-transcriber
#
################################################################################

WA_TRANSCRIBER_VERSION = local
WA_TRANSCRIBER_SITE = $(BR2_EXTERNAL_WAT_PATH)/prebuilt
WA_TRANSCRIBER_SITE_METHOD = local
WA_TRANSCRIBER_LICENSE = MIT
WA_TRANSCRIBER_LICENSE_FILES =

# The binary is built by the Go toolchain outside Buildroot (see
# "make -C os prebuilt"), never here: no golang-package, no cgo.
# Which cross-built binary: armv7 for the Pi 2/3 and the BeagleBone, armv6 for
# the Pi 1/Zero (ARM1176). The defconfig picks it.
WA_TRANSCRIBER_ARCH = $(call qstrip,$(BR2_PACKAGE_WA_TRANSCRIBER_ARCH))

WA_TRANSCRIBER_INSTALL_TARGET_CMDS = \
	$(INSTALL) -D -m 0755 $(@D)/wa-transcriber-linux-$(WA_TRANSCRIBER_ARCH) \
		$(TARGET_DIR)/usr/bin/wa-transcriber

$(eval $(generic-package))
