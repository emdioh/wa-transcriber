# Images (Buildroot)

Flashable SD-card images for the **Raspberry Pi 1/Zero**, the **Raspberry
Pi 2/3**, and the **BeagleBone
Black**, built with Buildroot from a `BR2_EXTERNAL` tree in this directory.

Milestone M6 in [../docs/PLAN.md](../docs/PLAN.md); the hardware validation
checklist is [../docs/hardware.md](../docs/hardware.md).

## What is in the image

| Partition | Filesystem | Content |
|---|---|---|
| 1 (boot) | FAT | firmware, kernel, device tree, `cmdline.txt`/`extlinux`, **`wat.txt`** |
| 2 (root) | ext4, **read-only** | the whole system, `wa-transcriber` in `/usr/bin` |
| 3 (DATA) | ext4, label `DATA` | `/data`: session, `config.json`, `wifi.json` — **created at first boot** |

Nothing is written to partition 2; `/tmp`, `/run`, `/var/log` and
`/var/lib/misc` are tmpfs. That is what makes the device survive power cuts.

Services, all started from `board/common/rootfs-overlay`:

| Script | Role |
|---|---|
| `S00boot` | mounts partition 1 read-only at `/boot`, so the binary and `S02console` can read `wat.txt` |
| `S01data` | creates and formats partition 3 on first boot, mounts `/data` (idempotent: a power cut mid-way cannot brick the card) |
| `S02console` | optional debug login shell, off unless `wat.txt` says `console=serial`, `usb` or `both` |
| `S40ntpd` | busybox `ntpd` against two servers; the binary waits for a sane clock anyway |
| `S90watchdog` | loads `bcm2835_wdt` / `omap_wdt` |
| `inittab` | `::respawn:` the transcriber with `-data /data -network managed -listen :80 -watchdog /dev/watchdog` — **not used on a board that boots with `init=`**, see "Init mode" below |

The watchdog is fed by the binary itself (5 s interval). `kill -STOP` the
process and the board reboots within ~20 s: that is the recovery path.

## Init mode

The BeagleBone Black image boots the transcriber as the machine's init
(`init=/usr/bin/wa-transcriber` in `boot/extlinux/extlinux.conf`), so the
kernel execs it directly and `/etc/inittab`, `rcS` and every script in
`rootfs-overlay/etc/init.d` are skipped. `internal/boot` does what they did —
mounts, hostname, watchdog module, `/boot`, `/data` — and
`internal/proc` reaps the children nobody waits for, because PID 1 is the only
reaper on the machine. Architecture: [../docs/ARCHITECTURE.md](../docs/ARCHITECTURE.md)
D19 and section 12. The settings `/etc/inittab` used to pass are the defaults
in `cmd/wa-transcriber/initmode.go`, and the kernel command line overrides them:

```txt
append root=/dev/mmcblk0p2 ro rootwait ... init=/usr/bin/wa-transcriber wat.network=none
```

**Rollback without a reflash:** mount the boot partition on any machine and
delete the `init=` token. The kernel then starts busybox init again and the old
script path runs, exactly as before D19. The other boards still boot that way
(the Raspberry Pi images need their own first-boot story for `/data` before
`cmdline.txt` gets the token).

## Debug console

The images ship no login shell: the kernel console is the board's UART and
nothing else listens there. A shell is opt-in through `wat.txt`:

| `console=` | Shell on |
|---|---|
| `all` (what these images ship) | serial + USB + HDMI |
| `off` | nothing |
| `serial` | the UART header: `/dev/ttyS0` on the BeagleBone (J1, 115200 8N1, 3.3 V USB-TTL adapter), `/dev/ttyAMA0` on the Raspberry Pi 1/2 |
| `usb` | the USB client port as a CDC-ACM gadget, `/dev/ttyGS0`; on a Linux host it appears as `/dev/ttyACM0` |
| `hdmi` | `tty1`, i.e. the HDMI output driven by a USB keyboard on the host port |
| `both` | `serial` + `usb` |
| `all` | `serial` + `usb` + `hdmi` |

`S02console` starts a busybox `getty` in a respawn loop and logs every
decision to `/dev/kmsg`, so the kernel prints it on every active console.
The template ships `console=all` while the boot and network work is being
validated. **The root account has no password**, so anyone who can reach one
of those ports has a root shell: set `console=off` before the device is used
by anyone else. The USB
path needs the port in peripheral mode (`ls /sys/class/udc`); the BeagleBone
has `CONFIG_USB_CONFIGFS_ACM=y` built in, the Raspberry Pies have no device
controller and logs `no USB device controller`.

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
make -C os bbb       # -> os/_output/bbb/images/sdcard.img
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
- Kernel drivers: `board/common/linux.fragment`.
- WiFi firmware: `BR2_PACKAGE_LINUX_FIRMWARE` in the defconfigs, trimmed to
  the two drivers the fragment enables (`MEDIATEK_MT76X2E` for
  `mt7662*.bin`, `RTL_81XX` for `rtlwifi/rtl8192eu_nic.bin`). A driver
  without its blob probes and fails (`Fatal - failed to load firmware`, no
  `wlan0`), so `make -C os check` refuses a defconfig that enables one of
  those drivers without the matching firmware package. `post-build.sh`
  links `mediatek/mt7662*.bin` at the top of `/lib/firmware`, where the
  6.1 kernel asks for it.
- Busybox applets: `board/common/busybox.fragment`.
- Partition layout: `board/<board>/genimage.cfg`.
- Anything in the root filesystem: drop the file under
  `board/common/rootfs-overlay/`.

After changing a defconfig by hand, run `make -C os check` and then rebuild.
