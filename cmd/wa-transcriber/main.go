// Command wa-transcriber links to WhatsApp as a companion device and
// transcribes voice notes. See docs/DESIGN.md.
package main

import (
	"context"
	"errors"
	"flag"
	"fmt"
	"log/slog"
	"os"
	"os/signal"
	"path/filepath"
	"runtime/debug"
	"syscall"
	"time"

	"github.com/emdioh/wa-transcriber/internal/bot"
	"github.com/emdioh/wa-transcriber/internal/config"
	"github.com/emdioh/wa-transcriber/internal/logbuf"
	"github.com/emdioh/wa-transcriber/internal/status"
	"github.com/emdioh/wa-transcriber/internal/wa"
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
	showVersion := flag.Bool("version", false, "print the version and exit")
	flag.Parse()
	if *showVersion {
		fmt.Println(version)
		return nil
	}

	memTotal := logbuf.MemTotal()
	setMemoryLimit(memTotal)
	logs := logbuf.New(logbuf.SizeForMemory(memTotal), os.Stderr)
	var level slog.Level
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

	b := &bot.Bot{
		WA:      client,
		Config:  cfg,
		Status:  st,
		Addrs:   status.Addresses,
		Version: version,
		Started: started,
		Log:     log,
	}
	b.Reset = func(ctx context.Context) error {
		client.Logout(ctx)
		if err := cfg.Reset(); err != nil {
			log.Error("erasing config", "err", err)
		}
		// TODO(netmgr): erase saved WiFi networks.
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

func envOr(name, def string) string {
	if v := os.Getenv(name); v != "" {
		return v
	}
	return def
}
