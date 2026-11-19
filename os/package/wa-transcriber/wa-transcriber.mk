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
WA_TRANSCRIBER_INSTALL_TARGET_CMDS = \
	$(INSTALL) -D -m 0755 $(@D)/wa-transcriber-linux-armv7 \
		$(TARGET_DIR)/usr/bin/wa-transcriber

$(eval $(generic-package))
