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
	"syscall"
	"time"

	"github.com/emdioh/wa-transcriber/internal/bot"
	"github.com/emdioh/wa-transcriber/internal/config"
	"github.com/emdioh/wa-transcriber/internal/logbuf"
	"github.com/emdioh/wa-transcriber/internal/netmgr"
	"github.com/emdioh/wa-transcriber/internal/status"
	"github.com/emdioh/wa-transcriber/internal/transcribe"
	"github.com/emdioh/wa-transcriber/internal/wa"
	"github.com/emdioh/wa-transcriber/internal/web"
)

// version is set at build time with -ldflags "-X main.version=...".
var version = "dev"

func main() {
	if err := run(); err != nil {
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
	showVersion := flag.Bool("version", false, "print the version and exit")
	flag.Parse()
	if *showVersion {
		fmt.Println(version)
		return nil
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
	log.Info("starting", "version", version, "data", *dataDir, "mem_total_mb", memTotal>>20)

	if err := os.MkdirAll(*dataDir, 0o700); err != nil {
		return err
	}
	cfg, err := config.Open(filepath.Join(*dataDir, "config.json"))
	if err != nil {
		return err
	}

	ctx, stop := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
	defer stop()

	nm, err := netmgr.New(*networkMode)
	if err != nil {
		return err
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
	if s := tr.Settings(); s.Configured() {
		log.Info("transcription configured", "provider", s.Provider, "model", s.Model,
			"translate_to", s.TranslateTo, "keep", s.Keep)
	} else {
		log.Warn("transcription not configured: set the provider and API key " +
			"(web UI, config.json or WAT_PROVIDER / WAT_API_KEY)")
	}

	b := &bot.Bot{
		WA:          client,
		Config:      cfg,
		Status:      st,
		Transcriber: tr,
		Addrs:       status.Addresses,
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
		return restart(log)
	}
	unlink := func(ctx context.Context) error {
		client.Logout(ctx)
		return restart(log)
	}
	client.OnMessage = func(m *bot.Message) { b.Handle(ctx, m) }
	client.OnLoggedOut = func() {
		// Unlinked from the phone: drop the session, keep the settings, pair again.
		client.Logout(ctx)
		if err := restart(log); err != nil {
			log.Error("restart after logout failed", "err", err)
			stop()
		}
	}

	if *listen != "" {
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
		})
		go func() {
			log.Info("web UI listening", "addr", *listen)
			// A busy port must not stop transcription: log and carry on.
			if err := ui.ListenAndServe(ctx, *listen); err != nil {
				log.Error("web UI stopped", "addr", *listen, "err", err)
			}
		}()
	}

	go func() {
		if err := client.Run(ctx, *pairPhone); err != nil && !errors.Is(err, context.Canceled) {
			log.Error("WhatsApp connection failed", "err", err)
			stop()
		}
	}()

	<-ctx.Done()
	log.Info("shutting down")
	client.Close()
	return nil
}

// restart re-executes the binary so it starts from a clean state (setup
// mode after a reset). Works the same on the image and in Docker.
func restart(log *slog.Logger) error {
	exe, err := os.Executable()
	if err != nil {
		return err
	}
	log.Warn("restarting")
	return syscall.Exec(exe, os.Args, os.Environ())
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

func envOr(name, def string) string {
	if v := os.Getenv(name); v != "" {
		return v
	}
	return def
}
