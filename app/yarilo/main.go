package main

import (
	"context"
	"flag"
	"log/slog"
	"os"
	"os/signal"
	"strings"
	"syscall"

	"github.com/yarilomail/yarilo/v2/internal/backend"
	"github.com/yarilomail/yarilo/v2/pkg/config"
)

var debug bool

func debugf(msg string, args ...any) {
	if debug {
		slog.Debug(msg, args...)
	}
}

func main() {
	slog.SetDefault(slog.New(slog.NewJSONHandler(os.Stdout, nil)).With("service", "yarilo"))

	// CONFIG and its default as in every other binary (#2428); -config, which
	// the image's entrypoint passes, still wins.
	cfgPath := flag.String("config", configPath(), "path to config file")
	flag.Parse()

	debug = strings.ToLower(os.Getenv("LOG_LEVEL")) == "debug"
	if debug {
		slog.SetDefault(slog.New(slog.NewJSONHandler(os.Stdout, &slog.HandlerOptions{
			Level: slog.LevelDebug,
		})).With("service", "yarilo"))
	}

	debugf("starting", "config", *cfgPath)

	cfg, err := config.Load(*cfgPath)
	if err != nil {
		slog.Error("failed to load config", "err", err, "path", *cfgPath)
		os.Exit(1)
	}
	if err := config.KeepBackendListeners(cfg); err != nil {
		slog.Error("listener config", "err", err)
		os.Exit(1)
	}

	srv, err := backend.New(cfg)
	if err != nil {
		slog.Error("failed to init server", "err", err)
		os.Exit(1)
	}

	ctx, stop := signal.NotifyContext(context.Background(), syscall.SIGINT, syscall.SIGTERM)
	defer stop()

	slog.Info("yarilo starting", "mode", cfg.Mode)
	if err := srv.Run(ctx); err != nil {
		slog.Error("server error", "err", err)
		os.Exit(1)
	}
	slog.Info("yarilo stopped")
}

func configPath() string {
	if p := os.Getenv("CONFIG"); p != "" {
		return p
	}
	return "/etc/yarilo/yarilo.yaml"
}
