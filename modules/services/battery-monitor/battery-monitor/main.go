// battery-monitor is a long-running user daemon that watches one or
// more batteries (laptop via UPower, Razer mouse via sysfs) and emits
// freedesktop notifications when they cross configured low / full
// thresholds.
//
// The daemon is configured by a JSON file path passed via --config.
// The Nix module emits that file into the nix store; see
// modules/services/battery-monitor.nix and DESIGN.md.
package main

import (
	"context"
	"flag"
	"fmt"
	"log/slog"
	"os"
	"os/signal"
	"syscall"

	"github.com/prismatic-koi/battery-monitor/internal/config"
	"github.com/prismatic-koi/battery-monitor/internal/daemon"
	"github.com/prismatic-koi/battery-monitor/internal/notify"
	"github.com/prismatic-koi/battery-monitor/internal/source"
	"github.com/prismatic-koi/battery-monitor/internal/source/razer"
	"github.com/prismatic-koi/battery-monitor/internal/source/upower"
)

func main() {
	if err := run(); err != nil {
		fmt.Fprintln(os.Stderr, "battery-monitor:", err)
		os.Exit(1)
	}
}

func run() error {
	var (
		configPath string
		logFormat  string
	)
	flag.StringVar(&configPath, "config", "", "path to JSON config file (required)")
	flag.StringVar(&logFormat, "log-format", "text",
		"log format: text (default, key=value, greppable) or json")
	flag.Parse()

	if configPath == "" {
		return fmt.Errorf("--config is required")
	}

	logger, err := newLogger(logFormat)
	if err != nil {
		return err
	}
	slog.SetDefault(logger)

	cfg, err := config.Load(configPath)
	if err != nil {
		return err
	}
	if len(cfg.Devices) == 0 {
		logger.Warn("no devices configured; nothing to do",
			"event", "startup_no_devices")
		return nil
	}

	notifier, err := notify.NewDBus()
	if err != nil {
		return fmt.Errorf("connect session bus: %w", err)
	}
	defer notifier.CloseConn()

	d := daemon.New(notifier, daemon.Options{
		Logger:  logger,
		AppName: "battery-monitor",
	})

	sources := make([]source.Source, 0, len(cfg.Devices))
	for _, dev := range cfg.Devices {
		switch dev.Kind {
		case config.KindLaptop:
			sources = append(sources, upower.New(dev.Name, logger))
		case config.KindRazer:
			sources = append(sources, razer.New(dev.Name, logger))
		default:
			return fmt.Errorf("device %q: unsupported kind %q", dev.Name, dev.Kind)
		}
	}

	ctx, cancel := signal.NotifyContext(context.Background(),
		syscall.SIGINT, syscall.SIGTERM)
	defer cancel()

	logger.Info("battery-monitor starting",
		"event", "startup",
		"devices", len(cfg.Devices))
	if err := d.Run(ctx, cfg.Devices, sources); err != nil && err != context.Canceled {
		return err
	}
	logger.Info("battery-monitor exiting",
		"event", "shutdown")
	return nil
}

// newLogger builds a slog.Logger for the requested format. We emit
// to stderr so systemd captures it into the journal. For both text
// and json formats the per-record `device=…` and `event=…` keys are
// preserved verbatim (slog renders attributes as `key=value` in text
// and as object fields in JSON), keeping the existing greppable
// shape from the Python script.
func newLogger(format string) (*slog.Logger, error) {
	switch format {
	case "text":
		return slog.New(slog.NewTextHandler(os.Stderr, nil)), nil
	case "json":
		return slog.New(slog.NewJSONHandler(os.Stderr, nil)), nil
	default:
		return nil, fmt.Errorf("--log-format: unknown %q (want text|json)", format)
	}
}
