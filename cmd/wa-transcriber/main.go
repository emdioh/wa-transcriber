// Command wa-transcriber links to WhatsApp as a companion device and
// transcribes voice notes. See docs/ARCHITECTURE.md.
package main

import (
	"context"
	"errors"
	"flag"
	"fmt"
	"log/slog"
	"net"
	"os"
	"os/signal"
	"path/filepath"
	"runtime/debug"
	"strconv"
	"syscall"
	"time"
	// Zone data in the binary: the scratch Docker image has no /usr/share/zoneinfo.
	_ "time/tzdata"

	"go.mau.fi/whatsmeow/types"

	"github.com/emdioh/wa-transcriber/internal/boot"
	"github.com/emdioh/wa-transcriber/internal/bot"
	"github.com/emdioh/wa-transcriber/internal/config"
	"github.com/emdioh/wa-transcriber/internal/coord"
	"github.com/emdioh/wa-transcriber/internal/logbuf"
	"github.com/emdioh/wa-transcriber/internal/netmgr"
	"github.com/emdioh/wa-transcriber/internal/proc"
	"github.com/emdioh/wa-transcriber/internal/status"
	"github.com/emdioh/wa-transcriber/internal/transcribe"
	"github.com/emdioh/wa-transcriber/internal/wa"
	"github.com/emdioh/wa-transcriber/internal/watchdog"
	"github.com/emdioh/wa-transcriber/internal/web"
)

// version and buildTime are set at build time with
// -ldflags "-X main.version=... -X main.buildTime=...". buildTime guards
// against a device clock that is not set yet (a bad clock breaks WhatsApp).
// updatePubKey (in update.go) is the base64 ed25519 public key of the release
// manifest; empty compiles self-updates out.
var (
	version   = "dev"
	buildTime string
)

// asInit is set by run: the kernel started this binary as init (D19).
var asInit bool

func main() {
	defer func() {
		if r := recover(); r != nil {
			if asInit {
				boot.Crash(slog.Default(), fmt.Errorf("panic: %v\n%s", r, debug.Stack()))
			}
			panic(r)
		}
	}()
	if err := run(); err != nil {
		if asInit {
			// An init that exits is a kernel panic ("Attempted to kill
			// init"); a device with no keyboard has to come back on its own.
			boot.Crash(slog.Default(), err)
		}
		fmt.Fprintln(os.Stderr, "wa-transcriber:", err)
		os.Exit(1)
	}
}

func run() error {
	dataDir := flag.String("data", envOr("WAT_DATA", "/data"), "directory for the session and config (env WAT_DATA)")
	pairPhone := flag.String("pair-phone", os.Getenv("WAT_PAIR_PHONE"),
		"pair with a code instead of a QR: phone number with country code (env WAT_PAIR_PHONE)")
	logLevel := flag.String("log-level", envOr("WAT_LOG_LEVEL", "info"), "debug, info, warn or error (env WAT_LOG_LEVEL)")
	listen := flag.String("listen", envOr("WAT_LISTEN", ":80"), "web UI address, empty to disable (env WAT_LISTEN)")
	networkMode := flag.String("network", envOr("WAT_NETWORK", "none"), "network management: none or managed (env WAT_NETWORK)")
	watchdogDev := flag.String("watchdog", os.Getenv("WAT_WATCHDOG"), "hardware watchdog device, empty to disable (env WAT_WATCHDOG)")
	claimMode := flag.Bool("coord-claims", os.Getenv("WAT_COORD_CLAIMS") == "1",
		"multi-transcriber claims: on only once the M2 experiment proves them invisible (env WAT_COORD_CLAIMS=1)")
	showVersion := flag.Bool("version", false, "print the version and exit")
	healthcheckOnly := flag.Bool("healthcheck", false,
		"probe the local web UI on -listen and exit 0 when healthy, 1 otherwise (for Docker HEALTHCHECK)")
	updateDir := flag.String("update-dir", os.Getenv("WAT_UPDATE_DIR"),
		"binary update directory, empty to disable self-updates (env WAT_UPDATE_DIR; the image passes /data/bin)")
	updateURL := flag.String("update-url",
		envOr("WAT_UPDATE_URL", "https://github.com/emdioh/wa-transcriber/releases/latest/download/manifest.json"),
		"release manifest URL (env WAT_UPDATE_URL; a test server can serve one on a board)")
	imageUpdate := flag.String("image-update", os.Getenv("WAT_IMAGE_UPDATE"),
		"data directory for image (OS) updates into the inactive root slot, empty to disable (env WAT_IMAGE_UPDATE; the image passes /data)")
	imageLayoutFile := flag.String("image-layout", envOr("WAT_IMAGE_LAYOUT", defaultImageLayout),
		"board description of the A/B layout: boot file and root partitions (env WAT_IMAGE_LAYOUT; the image ships /etc/wat/image.json)")
	flashDev := flag.String("flash", "",
		"install this card onto the board's internal storage (a device, empty = off) and exit instead of starting")
	flashConfirm := flag.String("flash-confirm", "",
		"the device name, to confirm -flash (an accidental -flash must not write a disk)")
	flashBoot := flag.String("flash-boot", "/boot", "boot partition for the migration marker of -flash")
	initFlag := flag.Bool("init", false,
		"run as the machine's init (D19): mount the filesystems, load the watchdog module, never exit")
	bootstrap := flag.String("bootstrap", os.Getenv("WAT_BOOTSTRAP"),
		"binary restart() re-execs: the factory binary that runs the boot gate, empty for this executable (env WAT_BOOTSTRAP)")
	flag.Parse()

	// Init mode: PID 1 alone is not enough (a container has that too), the
	// kernel command line has to name this binary as init.
	asInit = *initFlag || boot.InitRequested()
	if asInit {
		setInitEnv(os.LookupEnv, os.Setenv)
		applied, err := applyInitDefaults(flag.CommandLine, readKernelCmdline(), os.LookupEnv)
		if err != nil {
			return err
		}
		slog.Default().Info("init mode", "flags", applied)
	}
	if *showVersion {
		fmt.Println(version)
		return nil
	}
	if *healthcheckOnly {
		return healthcheck(*listen, nil)
	}
	if *flashDev != "" {
		// The install runs before the config, WhatsApp and the web UI: it
		// writes another disk and the device is about to be powered off, so
		// nothing else may be running and holding /data.
		if err := runFlashCmd(*flashDev, *flashConfirm, *flashBoot, *dataDir, *imageLayoutFile); err != nil {
			fmt.Fprintln(os.Stderr, "wa-transcriber:", err)
			os.Exit(1)
		}
		return nil
	}

	if asInit {
		// Order matters: /proc for the memory size below, /data for the
		// settings and the session, and the tmpfs that /etc/resolv.conf is a
		// symlink into before the network manager writes a name server.
		if err := boot.Setup(context.Background(), boot.Options{
			Log:          slog.Default(),
			HostnameFile: "/etc/hostname",
			Modules:      watchdogModules,
		}); err != nil {
			// Not fatal on purpose: the old init is still on the image and
			// knows how to bring this board up (see initFallback).
			initFallback(slog.Default(), err)
			return fmt.Errorf("init: %w", err)
		}
	}

	memTotal := logbuf.MemTotal()
	setMemoryLimit(memTotal)
	logs := logbuf.New(logbuf.SizeForMemory(memTotal), os.Stderr)
	level := new(slog.LevelVar) // changeable from the web UI
	if err := level.UnmarshalText([]byte(*logLevel)); err != nil {
		return fmt.Errorf("bad -log-level: %w", err)
	}
	log := slog.New(slog.NewTextHandler(logs, &slog.HandlerOptions{Level: level}))
	slog.SetDefault(log)
	if asInit {
		// One table for every child process: as init, a daemonised
		// wpa_supplicant is reparented to us and nothing else would reap it.
		// Attach the logger to the existing default table rather than
		// replacing it, so console gettys started in boot.Setup stay tracked.
		proc.SetLogger(func(format string, args ...any) {
			log.Info(fmt.Sprintf(format, args...))
		})
	}
	log.Info("starting", "version", version, "data", *dataDir, "listen", *listen,
		"mem_total_mb", memTotal>>20, "mem_limit_mb", memoryLimitMB(),
		"update_dir", *updateDir)

	// The boot gate runs before the data dir, the config and WhatsApp, exactly
	// as the kernel started it (D17). -version and -healthcheck returned above:
	// they must never exec anything. A failed exec only logs: the binary that is
	// running keeps running.
	key := resolveUpdateKey(log, updatePubKey)
	bootstrapStep(log, bootstrapRequest{
		UpdateDir: *updateDir,
		SelfPath:  executable(),
		Version:   version,
	}, key)

	// The second half of the boot gate: an image update counts the starts of
	// the slot it switched to and falls back to the other one after three
	// unconfirmed starts. Same place as update.Boot, for the same reason; it
	// never stops the process.
	imgReq := imageRequest{
		Dir:      *imageUpdate,
		Layout:   *imageLayoutFile,
		SysBlock: defaultSysBlock,
		DevDir:   defaultDevDir,
	}
	imgAct := imageBoot(imgReq, log, time.Now())
	if imgAct.Note != "" && imgAct.Note != "off" {
		log.Info("imageupdate: boot decision", "decision", imgAct.Note)
	}
	if imgAct.Reboot {
		// The boot file names the good slot again; only a reboot reaches it.
		log.Warn("imageupdate: rebooting into the previous slot")
		if err := restart(log, *bootstrap); err != nil {
			log.Error("imageupdate: reboot into the previous slot failed", "err", err)
		}
	}

	if err := os.MkdirAll(*dataDir, 0o700); err != nil {
		return err
	}
	if err := checkWritable(*dataDir); err != nil {
		return dataDirError(*dataDir, err)
	}
	cfg, err := config.Open(filepath.Join(*dataDir, "config.json"))
	if err != nil {
		return err
	}

	ctx, stop := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
	defer stop()
	if asInit {
		go proc.Default().ReapLoop(ctx, "/proc", time.Second)
	}

	nm, err := netmgr.NewWithData(*networkMode, *dataDir)
	if err != nil {
		return err
	}
	if *networkMode == "managed" && os.Geteuid() != 0 {
		log.Warn("network mode managed needs root: it changes interfaces and binds ports 53/67")
	}
	if err := nm.Start(ctx); err != nil {
		return err
	}
	log.Info("network manager started", "mode", *networkMode)

	started := time.Now()
	st := status.New(started)
	client, err := wa.Open(ctx, wa.Options{
		DBPath: filepath.Join(*dataDir, "session.db"),
		Log:    log,
		Status: st,
		QROut:  os.Stderr,
	})
	if err != nil {
		return err
	}

	tr := transcribe.New(cfg, client, log)
	ts := tr.Settings()
	// Effective settings without secrets: never the API key or the password.
	log.Info("effective settings",
		"reply_language", config.NormalizeLanguage(cfg.Get().Language),
		"provider", ts.Provider,
		"model", ts.Model,
		"keep_languages", ts.Keep,
		"translate_to", ts.TranslateTo,
		"network", *networkMode,
		"coord_claims", *claimMode)
	if !ts.Configured() {
		log.Warn("transcription not configured: set the provider and API key " +
			"(web UI, config.json or WAT_PROVIDER / WAT_API_KEY)")
	}

	// Multi-transcriber coordination. Claims stay off until the M2 experiment
	// on real phones proves they are invisible; observe-only mode (skip when
	// a peer already posted a tagged transcription) is always on.
	var signals coord.Signals
	if *claimMode {
		signals = client
	}
	co := &coord.Coordinator{
		Self:      client.Self,
		Signals:   signals,
		Log:       log,
		ClaimMode: *claimMode,
	}
	log.Info("coordination", "mode", map[bool]string{true: "claim", false: "observe-only"}[*claimMode])

	b := &bot.Bot{
		WA:          client,
		Config:      cfg,
		Status:      st,
		Transcriber: tr,
		Addrs:       status.Addresses,
		Network:     netAdapter{nm},
		Coord:       coordAdapter{co},
		WebPort:     listenPort(*listen),
		Version:     version,
		Started:     started,
		Log:         log,
	}
	b.Reset = func(ctx context.Context) error {
		client.Logout(ctx)
		if err := cfg.Reset(); err != nil {
			log.Error("erasing config", "err", err)
		}
		if err := nm.Erase(); err != nil {
			log.Error("erasing network settings", "err", err)
		}
		return restart(log, *bootstrap)
	}
	unlink := func(ctx context.Context) error {
		client.Logout(ctx)
		return restart(log, *bootstrap)
	}
	client.OnMessage = func(m *bot.Message) { b.Handle(ctx, m) }
	client.OnLoggedOut = func() {
		// Unlinked from the phone: drop the session, keep the settings, pair again.
		client.Logout(ctx)
		if err := restart(log, *bootstrap); err != nil {
			log.Error("restart after logout failed", "err", err)
			stop()
		}
	}

	// One update service for the whole process: the web card and the daily
	// check share its manifest cache, so a background check that finds a
	// release is the same answer the card shows. nil when the feature is off.
	svc := newUpdateService(*updateDir, version, *updateURL,
		filepath.Join(*dataDir, "updates", "check.json"), key, tr.Busy, log)
	if svc != nil {
		log.Info("binary updates enabled", "dir", *updateDir, "url", *updateURL)
	}
	// The image update shares the manifest and the key with the binary
	// update and nothing else: it writes a partition, not a file. nil when
	// -image-update is empty; a device whose layout has one root still gets
	// a service, because the card has to say "reflash once".
	imgSvc := newImageService(imgReq, version, *updateURL, key, tr.Busy, log)
	if imgSvc != nil {
		log.Info("image updates enabled", "data", *imageUpdate, "layout", *imageLayoutFile)
	}

	if *listen != "" {
		var updater web.Updater
		if svc != nil {
			updater = svc
		}
		var imageUpdater web.ImageUpdater
		if imgSvc != nil {
			imageUpdater = imgSvc
		}
		var flashInstaller web.FlashInstaller
		if flSvc := newFlashService(imgReq, flashPaths{
			SysBlock: defaultSysBlock,
			DevDir:   defaultDevDir,
			BootDir:  "/boot",
			Mounts:   "/proc/self/mounts",
		}, log); flSvc != nil {
			flashInstaller = flSvc
		}
		ui := web.New(web.Options{
			Config:         cfg,
			Status:         st,
			Logs:           logs,
			LogLevel:       level,
			WA:             client,
			Unlink:         unlink,
			Reset:          b.Reset,
			Addrs:          status.Addresses,
			Version:        version,
			Log:            log,
			Network:        nm,
			NetworkManaged: *networkMode == "managed",
			InAPMode:       nm.InAPMode,
			Updater:        updater,
			ImageUpdater:   imageUpdater,
			FlashInstaller: flashInstaller,
			// The backup card reads and rewrites the device state.
			DataDir: *dataDir,
			Restart: func() {
				if err := restart(log, *bootstrap); err != nil {
					log.Error("restart after the update failed", "err", err)
				}
			},
		})
		go func() {
			log.Info("web UI listening", "addr", *listen)
			// A busy port must not stop transcription: log and carry on.
			if err := ui.ListenAndServe(ctx, *listen); err != nil {
				log.Error("web UI stopped", "addr", *listen, "err", err)
			}
		}()
	}

	// A hung process stops petting the watchdog and the board reboots: the
	// only recovery path on a device without a keyboard.
	if *watchdogDev != "" {
		if wd, err := watchdog.Open(*watchdogDev, log); err != nil {
			log.Warn("watchdog not available, continuing without it", "device", *watchdogDev, "err", err)
		} else {
			log.Info("hardware watchdog armed", "device", *watchdogDev)
			go wd.Run(ctx)
		}
	}

	go func() {
		managed := *networkMode == "managed"
		// The first connect needs a resolver: an attempt that runs before
		// DHCP wrote /etc/resolv.conf fails on "[::1]:53" and only looks
		// like a fault in the pairing code.
		netmgr.WaitNetwork(ctx, netmgr.NetWait{
			Managed:  managed,
			InAPMode: nm.InAPMode,
			Log:      log,
		})
		if asInit {
			// No inittab, so the time daemon S40ntpd used to start is ours.
			startNTP(ctx, log)
		}
		if !netmgr.WaitClock(ctx, netmgr.ClockCheck{
			Managed:   managed,
			Now:       time.Now,
			BuildTime: parseBuildTime(buildTime),
			Synced:    netmgr.NTPSynced,
			Log:       log,
		}) {
			st.SetClockSynced(false)
		}
		if err := client.Run(ctx, *pairPhone); err != nil && !errors.Is(err, context.Canceled) {
			log.Error("WhatsApp connection failed", "err", err)
			stop()
		}
	}()

	// The update loop: the health timer 60 s after start, then the opt-in daily
	// check (never fatal, never while a note is queued or the setup AP runs).
	var check *dailyCheck
	if svc != nil {
		check = &dailyCheck{
			svc:     svc,
			log:     log,
			cfg:     updateSettingsAdapter{cfg},
			busy:    tr.Busy,
			inAP:    nm.InAPMode,
			now:     time.Now,
			first:   updateFirstCheck,
			every:   updateInterval,
			restart: func() { logAutoRestart(log, *bootstrap) },
		}
		// A typed nil in the interface would not be nil: only hand the image
		// half over when there is one.
		if imgSvc != nil {
			check.img = imgSvc
		}
	}
	startUpdateRuntime(ctx, log, *updateDir, check)

	<-ctx.Done()
	log.Info("shutting down")
	client.Close()
	return nil
}

// restart re-executes the bootstrap binary so it starts from a clean state
// (setup mode after a reset, or the boot gate promoting a pending version).
// Works the same on the image and in Docker. It must never exec
// os.Executable() of an updated binary: that would skip the gate.
func restart(log *slog.Logger, bootstrap string) error {
	exe, err := bootstrapPath(bootstrap)
	if err != nil {
		return err
	}
	log.Warn("restarting", "path", exe)
	return execFunc(exe, os.Args, os.Environ())
}

// logAutoRestart is the restart of an automatic install: a goroutine, like the
// web UI's, so no caller holds a lock while the process is replaced.
func logAutoRestart(log *slog.Logger, bootstrap string) {
	go func() {
		if err := restart(log, bootstrap); err != nil {
			log.Error("restart after the automatic update failed", "err", err)
		}
	}()
}

// setMemoryLimit applies a soft Go heap limit of a quarter of RAM, clamped to
// 32–256 MB, unless GOMEMLIMIT is set. It keeps 128 MB boards comfortable.
func setMemoryLimit(memTotal uint64) {
	if os.Getenv("GOMEMLIMIT") != "" || memTotal == 0 {
		return
	}
	limit := min(max(memTotal/4, 32<<20), 256<<20)
	debug.SetMemoryLimit(int64(limit))
}

// memoryLimitMB is the soft heap limit currently in effect (GOMEMLIMIT
// included). debug.SetMemoryLimit(-1) returns it without changing it.
func memoryLimitMB() int64 { return debug.SetMemoryLimit(-1) >> 20 }

// listenPort extracts the port of a -listen address ("" when disabled).
func listenPort(addr string) string {
	if addr == "" {
		return ""
	}
	if _, port, err := net.SplitHostPort(addr); err == nil && port != "" {
		return port
	}
	return "80"
}

// updateSettingsAdapter gives the daily check the config store's effective
// settings without making the check depend on the whole store.
type updateSettingsAdapter struct{ cfg *config.Store }

func (a updateSettingsAdapter) UpdateSettings() (checkMode string, auto bool) {
	return a.cfg.Get().UpdateSettings(os.Getenv)
}

// executable is os.Executable() without the error: a path that cannot be
// resolved only means the boot gate cannot tell factory from /data.
func executable() string {
	exe, err := os.Executable()
	if err != nil {
		return ""
	}
	return exe
}

// coordAdapter exposes the coordinator to the bot without making bot depend
// on the coord package.
type coordAdapter struct{ c *coord.Coordinator }

func (a coordAdapter) Acquire(ctx context.Context, v bot.Voice) bot.Lease {
	lease := a.c.Acquire(ctx, coord.Voice{
		Chat:    v.Chat,
		ID:      v.Quote.ID,
		Sender:  v.Quote.Sender,
		FromMe:  v.FromMe,
		Seconds: v.Seconds,
	})
	if lease == nil {
		return nil
	}
	return lease
}

func (a coordAdapter) ObserveClaim(chat types.JID, voiceID string, from types.JID, at time.Time) {
	a.c.ObserveClaim(chat, voiceID, from, at)
}

func (a coordAdapter) ObserveTranscription(chat types.JID, voiceID string, from types.JID) {
	a.c.ObserveTranscription(chat, voiceID, from)
}

func (a coordAdapter) Counters() (claims, stoodDown, tookOver uint64, mode string) {
	return a.c.Counters()
}

// netAdapter exposes the network manager to the bot without making bot depend
// on the netmgr package.
type netAdapter struct{ m netmgr.Manager }

func (a netAdapter) Interfaces() []bot.NetIface {
	st := a.m.Status()
	out := make([]bot.NetIface, 0, len(st.Interfaces))
	for _, i := range st.Interfaces {
		out = append(out, bot.NetIface{
			Name: i.Name, Kind: string(i.Kind), SSID: i.SSID,
			APClients: i.APClients, IPs: i.IPs,
		})
	}
	return out
}

// parseBuildTime accepts RFC 3339 or a Unix timestamp ("", "0" or garbage
// gives the zero time, which disables the check).
func parseBuildTime(s string) time.Time {
	if t, err := time.Parse(time.RFC3339, s); err == nil {
		return t
	}
	if n, err := strconv.ParseInt(s, 10, 64); err == nil && n > 0 {
		return time.Unix(n, 0)
	}
	return time.Time{}
}

func envOr(name, def string) string {
	if v := os.Getenv(name); v != "" {
		return v
	}
	return def
}
