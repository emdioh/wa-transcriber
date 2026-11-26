// Package boot is what the program does first when the kernel starts it as
// init (`init=/usr/bin/wa-transcriber` in the bootloader command line): mount
// the filesystems a read-only root does not carry, find and mount /data, load
// the drivers the board's device tree does not autoload. After it, the program
// is the whole userland — there is no inittab, no rcS and no shell, which is
// the point: docs/ARCHITECTURE.md, D19.
//
// Everything here is idempotent. A re-exec (an update, a reset) runs it again
// with the mounts already in place, and a board that boots with the old
// busybox init still works, because in that case nothing calls this package at
// all.
package boot

import (
	"context"
	"fmt"
	"log/slog"
	"os"
	"os/exec"
	"strings"
	"time"

	"github.com/emdioh/wa-transcriber/internal/proc"
)

const (
	// BootDir is the FAT partition (p1) with the bootloader files and
	// wat.txt, mounted read-only: nothing writes to it except the image
	// updater, and only while it remounts it itself.
	BootDir = "/boot"
	// DataDir is the data partition (p4, label DATA): the session, the
	// settings and everything else that must survive a power cut.
	DataDir = "/data"
	// DataLabel is the filesystem label of the data partition. The label is
	// the identity: partition numbers move when a card is copied, and the two
	// root slots must never be mistaken for data.
	DataLabel = "DATA"
)

// Options describes the machine. Zeros are the board's defaults, so a test
// only sets what it checks.
type Options struct {
	Log *slog.Logger

	// Mounts is the mount table to consult for "is it already mounted":
	// "/proc/self/mountinfo" on a board, a file of the same shape in a test.
	Mounts  string
	ProcDir string
	DevDir  string
	SysDir  string
	// Root is the mount point whose device identifies the running disk ("/").
	Root string
	// BootDir is the mount point of the FAT boot partition ("/boot").
	BootDir string
	// DataDir is the mount point of the DATA partition ("/data").
	DataDir string
	// MigrateMountDir is the temporary mount point used when a wat-migrate
	// marker names a `from /dev/...` device ("/run/wat-migrate").
	MigrateMountDir string
	// HostnameFile is read for the machine name ("" skips setting it).
	HostnameFile string
	// WatFile is the installer-edited file on the boot partition: the
	// console= line and the WiFi provisioning live there.
	WatFile string
	// ModelFile is the device tree's model string, used to pick the UART.
	ModelFile string
	// ConfigFS and UDCDir are where the USB gadget is built and bound.
	ConfigFS string
	UDCDir   string
	// Spawn starts the console login on a tty (nil: busybox getty in a
	// respawn loop).
	Spawn func(tty string)
	// IfUp brings an interface up (nil: netlink-free ioctl on Linux).
	IfUp func(name string) error
	// Modules are kernel modules to load, in order, stopping at the first one
	// that loads (the watchdog driver differs per board).
	Modules []string
	// HotplugHelper is written to /proc/sys/kernel/hotplug so USB devices
	// plugged in after boot trigger module loading (default "/sbin/mdev").
	HotplugHelper string
	// Wait is how long a freshly created partition may take to appear as a
	// block device. Zero means 10 s.
	Wait time.Duration

	Mount    func(source, target, fstype string, flags uintptr, data string) error
	Unmount  func(target string, flags int) error
	ReadFile func(string) ([]byte, error)
	// MkdirAll creates the mount point of a filesystem that is not there. The
	// image has every one of them (the root filesystem is read-only), so this
	// only matters on a machine where the package is exercised by a test.
	MkdirAll func(string, os.FileMode) error
	// Run executes a helper (modprobe, sfdisk, mkfs.ext4) with optional stdin.
	Run func(ctx context.Context, stdin, name string, args ...string) error

	// Unexported hooks for unit tests to observe removal ordering without
	// depending on filesystem permissions.
	removeFile func(string) error
	removeAll  func(string) error
}

// IsInit reports whether this process is PID 1, which is necessary but not
// sufficient to be the machine's init: a container's main process is PID 1 as
// well. Use InitRequested for the decision.
func IsInit() bool { return os.Getpid() == 1 }

// CmdlineNamesUs reports whether a kernel command line names this binary as
// init.
func CmdlineNamesUs(cmdline string) bool {
	for _, arg := range strings.Fields(cmdline) {
		if v, ok := strings.CutPrefix(arg, "init="); ok && strings.Contains(v, "wa-transcriber") {
			return true
		}
	}
	return false
}

// InitRequested reports whether the kernel started this binary as the
// machine's init, and it is the first thing the program asks.
//
// It cannot assume that /proc is mounted: mounting /proc is one of the things
// init exists to do, and on a board that boots with init= nothing else has run.
// Without this mount, /proc/cmdline does not exist, the answer is "no", and the
// program goes on to fail on the read-only root with "Attempted to kill init"
// after trying to write to a /data nobody mounted (a board run, and the reason
// this function exists). PID 1 alone is not the answer either: a container's
// main process is PID 1 too, and its /proc/cmdline is the host's.
func InitRequested() bool {
	if !IsInit() {
		return false
	}
	if _, err := os.Stat("/proc/cmdline"); err != nil {
		if err := mount("proc", "/proc", "proc", 0, ""); err != nil {
			return false
		}
	}
	b, err := os.ReadFile("/proc/cmdline")
	if err != nil {
		return false
	}
	return CmdlineNamesUs(string(b))
}

func (o Options) withDefaults() Options {
	if o.Log == nil {
		o.Log = slog.Default()
	}
	if o.Mounts == "" {
		o.Mounts = "/proc/self/mountinfo"
	}
	if o.ProcDir == "" {
		o.ProcDir = "/proc"
	}
	if o.DevDir == "" {
		o.DevDir = "/dev/"
	}
	if o.SysDir == "" {
		o.SysDir = "/sys"
	}
	if o.Root == "" {
		o.Root = "/"
	}
	if o.BootDir == "" {
		o.BootDir = BootDir
	}
	if o.DataDir == "" {
		o.DataDir = DataDir
	}
	if o.MigrateMountDir == "" {
		o.MigrateMountDir = "/run/wat-migrate"
	}
	if o.Wait <= 0 {
		o.Wait = 10 * time.Second
	}
	if o.Mount == nil {
		o.Mount = mount
	}
	if o.Unmount == nil {
		o.Unmount = unmount
	}
	if o.ReadFile == nil {
		o.ReadFile = os.ReadFile
	}
	if o.MkdirAll == nil {
		o.MkdirAll = os.MkdirAll
	}
	if o.Run == nil {
		o.Run = runCmd
	}
	if o.removeFile == nil {
		o.removeFile = os.Remove
	}
	if o.removeAll == nil {
		o.removeAll = os.RemoveAll
	}
	if o.WatFile == "" {
		o.WatFile = "/boot/wat.txt"
	}
	if o.ModelFile == "" {
		o.ModelFile = "/proc/device-tree/model"
	}
	if o.ConfigFS == "" {
		o.ConfigFS = "/sys/kernel/config"
	}
	if o.UDCDir == "" {
		o.UDCDir = "/sys/class/udc"
	}
	if o.HotplugHelper == "" {
		o.HotplugHelper = "/sbin/mdev"
	}
	return o
}

// Setup brings the machine to the point where the rest of the program can run
// as if it had been started by an init system.
//
// The order is not free: /dev has to exist before the tmpfs children below it,
// and /tmp before anything writes a name server (on this image
// /etc/resolv.conf is a symlink into /tmp, so a DHCP client cannot write it
// until the tmpfs is there).
func Setup(ctx context.Context, o Options) error {
	o = o.withDefaults()

	for _, m := range pseudoMounts() {
		if err := o.ensureMount(m); err != nil {
			return err
		}
	}
	o.setHostname()
	// Nothing else brings lo up on this image, and without it a health check
	// on 127.0.0.1 answers "network unreachable".
	if err := o.ifUp("lo"); err != nil {
		o.Log.Warn("cannot bring lo up", "err", err)
	}
	// An unmounted boot partition only means the installer-written wat.txt is
	// not read and an image update is unavailable: the device still works, so
	// this is a warning, not a failure.
	if err := o.mountBoot(ctx); err != nil {
		o.Log.Warn("boot partition not mounted, wat.txt is ignored", "err", err)
	}
	o.loadModules(ctx)
	o.registerHotplug()
	o.coldplugUSB(ctx)
	// Before /data: the debug shell is how a board whose data partition is
	// broken is still reachable.
	o.setupConsole(ctx)
	return o.mountData(ctx)
}

// ifUp brings a network interface up.
func (o Options) ifUp(name string) error {
	if o.IfUp != nil {
		return o.IfUp(name)
	}
	return interfaceUp(name)
}

func pseudoMounts() []mountSpec {
	const (
		nosuid = msNoSuid
		nodev  = msNoDev
	)
	return []mountSpec{
		{source: "proc", target: "/proc", fstype: "proc"},
		{source: "sysfs", target: "/sys", fstype: "sysfs"},
		// CONFIG_DEVTMPFS_MOUNT=y has the kernel do this one; the entry stays
		// because a board kernel without it would otherwise have no /dev at
		// all (and the failure would look like "no block devices").
		{source: "devtmpfs", target: "/dev", fstype: "devtmpfs"},
		{source: "devpts", target: "/dev/pts", fstype: "devpts", data: "gid=5,mode=620"},
		{source: "tmpfs", target: "/dev/shm", fstype: "tmpfs", flags: nosuid | nodev, data: "mode=1777,size=16M"},
		{source: "tmpfs", target: "/tmp", fstype: "tmpfs", flags: nosuid | nodev, data: "size=64M"},
		{source: "tmpfs", target: "/run", fstype: "tmpfs", flags: nosuid | nodev, data: "size=32M"},
		{source: "tmpfs", target: "/var/log", fstype: "tmpfs", flags: nosuid | nodev, data: "size=8M"},
		{source: "tmpfs", target: "/var/lib", fstype: "tmpfs", flags: nosuid | nodev, data: "size=8M"},
		{source: "tmpfs", target: "/var/lib/misc", fstype: "tmpfs", flags: nosuid | nodev, data: "size=8M"},
	}
}

// setHostname applies /etc/hostname. The kernel starts every boot with the
// board's default name and nothing else would ever set it.
func (o Options) setHostname() {
	if o.HostnameFile == "" {
		return
	}
	b, err := o.ReadFile(o.HostnameFile)
	if err != nil {
		return
	}
	name := strings.TrimSpace(string(b))
	if name == "" {
		return
	}
	if err := setHostname(name); err != nil {
		o.Log.Warn("cannot set the host name", "name", name, "err", err)
		return
	}
	o.Log.Info("host name set", "name", name)
}

// loadModules loads the first module in Options.Modules that loads. The
// watchdog driver is the one that matters (M6): a board without it has no
// recovery path, so a failure is logged at warning level.
func (o Options) loadModules(ctx context.Context) {
	if len(o.Modules) == 0 {
		return
	}
	var last error
	for _, m := range o.Modules {
		if err := o.Run(ctx, "", "modprobe", m); err != nil {
			last = err
			continue
		}
		o.Log.Info("kernel module loaded", "module", m)
		return
	}
	o.Log.Warn("no watchdog module could be loaded", "modules", o.Modules, "err", last)
}

// runCmd runs a helper with an optional stdin and returns its output in the
// error, so a failure in a boot step is readable on the console.
func runCmd(ctx context.Context, stdin, name string, args ...string) error {
	cmd := exec.CommandContext(ctx, name, args...)
	if stdin != "" {
		cmd.Stdin = strings.NewReader(stdin)
	}
	out, err := proc.CombinedOutput(cmd)
	if err != nil {
		return fmt.Errorf("%s %s: %w: %s", name, strings.Join(args, " "), err,
			strings.TrimSpace(string(out)))
	}
	return nil
}
