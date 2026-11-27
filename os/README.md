# Images (Buildroot)

Flashable SD-card images for the **Raspberry Pi 1/Zero**, the **Raspberry
Pi 2/3**, the **BeagleBone Black**, and the **Cubieboard 1** (Allwinner A10),
built with Buildroot from a `BR2_EXTERNAL` tree in this directory.

Milestone M6 in [../docs/PLAN.md](../docs/PLAN.md); the hardware validation
checklist is [../docs/hardware.md](../docs/hardware.md).

## What is in the image

| Partition | Filesystem | Content |
|---|---|---|
| 1 (boot) | FAT | firmware, kernel, device tree, `cmdline.txt`/`extlinux`, **`wat.txt`** |
| 2 (root A) | ext4, **read-only** | barebones appliance rootfs (`wat-root-a`), `wa-transcriber` in `/usr/bin` |
| 3 (root B) | ext4, **read-only** | inactive A/B root slot (`wat-root-b`, empty on fresh image) |
| 4 (DATA) | ext4, label `DATA` | `/data`: session, `config.json`, `wifi.json` — **created at first boot** |

Nothing is written to the root partition; `/tmp`, `/run`, `/var/log`, `/var/lib`
and `/var/lib/misc` are tmpfs (and `/etc/resolv.conf` points to `/tmp/resolv.conf`).
That is what makes the device survive power cuts.

The rootfs is a **barebones appliance** with **no BusyBox, no `/bin/sh`, no
`/etc/inittab`, no `/etc/init.d/*`, no `getty`, no `kmod`/`mdev`, no `util-linux`,
no `dnsmasq`, no `iw`, and no `wpa_cli`**. The only userspace components shipped
alongside `/usr/bin/wa-transcriber` (PID 1) are:

- `e2fsprogs` (`mkfs.ext4`, `e2fsck`) for formatting and checking `/data`
- `wpa_supplicant` and `hostapd` (driven directly via nl80211 / unixgram socket)
- WiFi firmware (`mt7662*.bin`, `rtlwifi/rtl8192eu_nic.bin`), `wireless-regdb`, and CA certificates

All supported WiFi and hardware watchdog drivers (`rt2800usb`, `mt76x2u`,
`rtl8xxxu`, `bcm2835_wdt`, `omap_wdt`, `sunxi_wdt`) are compiled directly into
the kernel (`=y`) with `CONFIG_FW_LOADER=y`, so the kernel needs no `modprobe`
or `mdev`.

The watchdog is fed by the binary itself (5 s interval). `kill -STOP` the
process and the board reboots within ~20 s: that is the recovery path.

## Init mode

All four board images boot the transcriber directly as PID 1
(`init=/usr/bin/wa-transcriber` in `extlinux/extlinux.conf` on the BeagleBone
Black and Cubieboard 1, and in `cmdline.txt` on the Raspberry Pi 1 and 2).
`internal/boot` handles every boot task in pure Go — pseudo-filesystems and
tmpfs mounts, hostname, loopback bring-up, `/boot` mount, `/data` MBR creation
(`internal/part`) and ext4 mount/repair, `/boot/wat-migrate` eMMC migration,
and the built-in diagnostic console — while `internal/sntp` handles clock sync
and `internal/proc` reaps orphaned children. Architecture:
[../docs/ARCHITECTURE.md](../docs/ARCHITECTURE.md) D19 and section 12. The
board defaults live in `cmd/wa-transcriber/initmode.go`, and the kernel command
line overrides any flag as `wat.<flag>=<value>`:

```txt
append root=/dev/mmcblk0p2 ro rootwait ... init=/usr/bin/wa-transcriber wat.network=none
```

## Diagnostic console

There is no login shell, no BusyBox, and no `/bin/sh` on the image. An opt-in
**built-in read-only diagnostic line console** (implemented in pure Go inside
`internal/boot`) can be enabled through `wat.txt`:

| `console=` | Console on |
|---|---|
| `all` (what these images ship) | serial + USB + HDMI |
| `off` | nothing |
| `serial` | the UART header: `/dev/ttyS0` on the BeagleBone (J1, 115200 8N1, 3.3 V USB-TTL adapter) and Cubieboard 1 (UART0), `/dev/ttyAMA0` on the Raspberry Pi 1/2 |
| `usb` | the USB client port as a CDC-ACM gadget, `/dev/ttyGS0`; on a Linux host it appears as `/dev/ttyACM0` |
| `hdmi` | `tty1`, i.e. the HDMI output driven by a USB keyboard on the host port |
| `both` | `serial` + `usb` |
| `all` | `serial` + `usb` + `hdmi` |

When enabled, `wa-transcriber` opens the requested tty directly (and creates
the USB CDC-ACM gadget via configfs for `usb`) and presents a `wat> ` prompt
with read-only inspection commands (`help`, `uptime`, `mem`, `mounts`, `ip`,
`ps`) plus `reboot`. There is no shell and no arbitrary command execution. Set
`console=off` to disable the console completely. The USB path needs the port in
peripheral mode (`ls /sys/class/udc`); the BeagleBone has
`CONFIG_USB_CONFIGFS_ACM=y` built in, while the Raspberry Pies have no device
controller and log `no USB device controller`.

Kernel messages never need `console=`: the BeagleBone boots with
`earlycon=uart8250,mmio32,0x44e09000,115200n8 console=ttyS0,115200
console=tty1` and no `quiet`, so the boot log appears on the serial header
**and** on an HDMI monitor from the first line. That is the cheapest way to
see whether the card booted at all: a BeagleBone that shows nothing on HDMI
after power-up is not running this image (a blank screen with a bare blinking
cursor is usually the on-board eMMC system: hold **S2** while powering to force
the microSD).

## Building

Requirements: ~15 GB of disk, network access for the Buildroot download and
the Buildroot source cache, and the usual host tools (`make`, `gcc`, `bison`,
`flex`, `rsync`, `cpio`, `unzip`, `bc`, `perl`).

```bash
make -C os check     # structural checks, no network, seconds
make -C os rpi1      # -> os/_output/rpi1/images/sdcard.img   (Pi 1/Zero, ARMv6)
make -C os rpi2      # -> os/_output/rpi2/images/sdcard.img   (Pi 2/3/Zero 2, ARMv7)
make -C os bbb       # -> os/_output/bbb/images/sdcard.img    (BeagleBone Black, ARMv7)
make -C os cubie     # -> os/_output/cubie/images/sdcard.img  (Cubieboard 1, Allwinner A10, ARMv7)
```

The first build compiles a toolchain, the kernel and U-Boot and takes about
an hour on 12 cores; later builds reuse the download cache in
`os/_buildroot/` and the per-board `os/_output/<board>/` trees.

Measured with Buildroot 2024.02.9 on 12 cores:

| Board | `sdcard.img` | gzipped | boot partition |
|---|---|---|---|
| Raspberry Pi 1/Zero | 192 MB | ~38 MB | bootcode.bin, start.elf, fixup.dat, zImage, the bcm2708 dtbs, cmdline.txt, config.txt, wat.txt |
| Raspberry Pi 2/3 | 192 MB | 38.6 MB | bootcode.bin, start.elf, fixup.dat, zImage, the bcm2709/bcm2710 dtbs, cmdline.txt, config.txt, wat.txt |
| BeagleBone Black | 192 MB | 50.2 MB | MLO, u-boot.img, zImage, am335x-boneblack.dtb, extlinux/, wat.txt |
| Cubieboard 1 (A10) | 353 MB (A/B) | — | raw `u-boot-sunxi-with-spl.bin` at 8K; FAT p1: zImage, sun4i-a10-cubieboard.dtb, extlinux/, wat.txt |

The image is 192 MB regardless of the card size: it is one 32 MB boot
partition plus a 160 MB root partition; the start of the card stays free for
the `DATA` partition created at first boot.

What the Makefile does, in order:

1. `check` — validates this tree (`check.sh`).
2. `buildroot` — downloads and extracts Buildroot **2024.02.9** (LTS), pinned.
3. `prebuilt` — `make -C .. build-arm7` and copies the static armv7 binary to
   `os/prebuilt/wa-transcriber-linux-armv7`. Go is never run by Buildroot.
4. Buildroot builds the board with `BR2_EXTERNAL=os`.
5. `mkimage.sh` runs the Buildroot-provided `genimage` with
   `board/<board>/genimage.cfg` to produce `sdcard.img`.

`make -C os distclean` removes everything it downloaded or built.

Publishing the images so devices can update themselves over the network is a
separate, ordered procedure: [../docs/image-release.md](../docs/image-release.md).

## Writing the card and first boot

```bash
# Linux/macOS (find the card first: lsblk / diskutil list)
sudo dd if=os/_output/rpi2/images/sdcard.img of=/dev/sdX bs=4M conv=fsync status=progress
```

On Windows use Raspberry Pi Imager ("Use custom image") or balenaEtcher.

Before ejecting, open the small FAT partition and edit **`wat.txt`**: set
`setup_ap_key` to your own WPA2 passphrase (≥ 8 characters), `country` to
your country code and, if you want a login shell on the board, `console=`
(see [Debug console](#debug-console)). The partition is mounted read-only at
`/boot` at boot, and the file is only read then.

`wat.txt` can also carry the WiFi network itself, which skips the setup AP
entirely on the first boot:

```ini
wifi_ssid=Casa
wifi_passphrase=change-me-too
```

The device imports both lines into `/data/wifi.json` when the network manager
starts (the passphrase becomes the WPA key and is never stored, and the
network appears on the Network page like one added there). Importing is
idempotent: an unchanged `wat.txt` does not reorder the saved networks, so a
network added later in the UI is not pushed behind it. A passphrase outside
8–63 characters is logged as a warning and ignored, and the device falls back
to the setup AP. **The boot partition is unencrypted**: anyone who takes the
card reads the passphrase, and it stays there after the import — `console=`
ships as `all` for the same reason (physical access), so treat a card with
`wifi_passphrase` in it as a secret.

First boot:

1. Power the board with no cable and no saved network. After ~30 s a WiFi
   network **`WA-Setup-XXXX`** appears (XXXX = last four hex digits of the
   MAC address).
2. Join it with the key from `wat.txt`. The captive portal opens the web UI
   by itself; if not, open `http://192.168.4.1/` or `http://wa.local/`.
3. Set the web UI password, link WhatsApp, then add your home WiFi on the
   Network page. The AP disappears and the device joins your network;
   `http://wa.local/` works from any computer on it.

With `wifi_ssid`/`wifi_passphrase` in `wat.txt` steps 1 and 2 do not happen:
the device joins the named network while it boots (Ethernet, if plugged, is
tried at the same time) and the UI is reachable at `http://wa.local/` from
that network.

`/data` (partition 3) is created on that first boot and is where everything
persistent lives: back it up by copying the partition (or at least
`session.db*` and `config.json`). Reflashing the image keeps partition 3 if
`dd` is pointed at the same `sdcard.img`? **No** — reflashing rewrites the
whole card. Restore the two files from your backup instead; without
`session.db` you must pair again.

## Changing the configuration

- Board configuration: `configs/wat_<board>_defconfig`. Two deliberate
  differences from Buildroot's own defconfigs: U-Boot is fetched from GitHub
  (`BR2_TARGET_UBOOT_CUSTOM_TARBALL`) because `ftp.denx.de` hangs on IPv6
  from some networks, and the toolchain is musl.
  (`make -C os menuconfig-<board>` to explore, then `savedefconfig`).
- Kernel drivers: `board/common/linux.fragment` (all WiFi and watchdog drivers
  compiled built-in `=y` with `CONFIG_FW_LOADER=y`).
- WiFi firmware: `BR2_PACKAGE_LINUX_FIRMWARE` in the defconfigs, trimmed to
  the two drivers the fragment enables (`MEDIATEK_MT76X2E` for
  `mt7662*.bin`, `RTL_81XX` for `rtlwifi/rtl8192eu_nic.bin`). A driver
  without its blob probes and fails (`Fatal - failed to load firmware`, no
  `wlan0`), so `make -C os check` refuses a defconfig that enables one of
  those drivers without the matching firmware package. `post-build.sh`
  links `mediatek/mt7662*.bin` at the top of `/lib/firmware`, where the
  6.1 kernel asks for it.
- Partition layout: `board/<board>/genimage.cfg`.
- Root filesystem post-processing: `board/common/post-build.sh` and
  `board/<board>/post-build.sh`.

After changing a defconfig by hand, run `make -C os check` and then rebuild.
