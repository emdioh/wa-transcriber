// Package imageupdate updates the OS image in place (M10, decision D18,
// ARCHITECTURE.md section 11): the boards keep two root partitions and an
// update writes the one that is not running, verifies it by reading it back,
// and moves the boot file to it.
//
// Layout: p1 boot (FAT, mounted read-only at /boot), p2 root A, p3 root B
// (ext4), p4 data. The boot file names the current root — extlinux/extlinux.conf
// on the BeagleBone Black, cmdline.txt on the Pi 2 — and is the only thing on
// the FAT partition that matters: rewriting it is the switch. /data is never
// touched by an image update, so an update never costs a re-pairing.
//
// The order every operation follows:
//
//	Install  write the inactive slot while hashing → fsync → read back →
//	         compare the sha256. The boot file is not touched.
//	Switch   one atomic rename of the boot file, plus image-boot.json on /data
//	         saying which slot owes a confirmation.
//	Confirm  60 s of health (the same timer as the binary update): the slot
//	         keeps its hash and stops counting.
//	Boot     3 unconfirmed starts: the boot file goes back and main reboots.
//
// A failure before Switch leaves the device running exactly what it runs now,
// with the other slot possibly half-written and the boot file untouched; a
// failure inside Switch leaves either the old or the new boot file, never a
// mixture; a failure after Switch is what the counter is for.
//
// The fallback is the *userspace* half of the health gate: it covers an image
// that boots and starts this program but whose app never becomes healthy (a
// broken configuration, a crash loop, a full /data). An image whose kernel or
// init never reaches this program cannot be caught here: that needs the
// bootloader's own counter (U-Boot bootcount), which is a separate M10 step
// and out of scope for this package.
//
// Every path is injected (Layout), so the whole package runs on temp files in
// a test: files stand in for partitions and a directory for /boot.
package imageupdate
