// yarilo-dict serves the named dicts declared in yarilo.yaml. It is the only
// process that links a dict engine: sessions name a dict and speak the dict
// protocol to this one (#1733).
package main

import (
	"context"
	"crypto/tls"
	"fmt"
	"log/slog"
	"net"
	"os"
	"os/signal"
	"syscall"

	"github.com/prometheus/client_golang/prometheus"

	"github.com/yarilomail/yarilo/internal/dictsrv"
	"github.com/yarilomail/yarilo/internal/telemetry"
	"github.com/yarilomail/yarilo/pkg/build"
	"github.com/yarilomail/yarilo/pkg/config"
	"github.com/yarilomail/yarilo/pkg/dict"
	_ "github.com/yarilomail/yarilo/pkg/dict/drivers/all" // the engines live here
	"github.com/yarilomail/yarilo/pkg/logging"
	"github.com/yarilomail/yarilo/pkg/mtls"
)

func main() {
	logging.Setup("dict")

	cfgPath := os.Getenv("CONFIG")
	if cfgPath == "" {
		cfgPath = "/etc/yarilo/yarilo.yaml"
	}
	cfg, err := config.Load(cfgPath)
	if err != nil {
		slog.Error("config load failed", "err", err, "path", cfgPath)
		os.Exit(1)
	}

	listen := cfg.DictService.DictListen
	if listen == "" {
		slog.Error("dict_service.dict_listen is required")
		os.Exit(1)
	}

	dicts, err := openDicts(cfg.Dicts)
	if err != nil {
		slog.Error("dict init failed", "err", err)
		os.Exit(1)
	}
	defer closeDicts(dicts)

	ln, err := buildListener(cfg, listen)
	if err != nil {
		slog.Error("listen failed", "err", err, "addr", listen)
		os.Exit(1)
	}

	slog.Info("yarilo-dict starting", "version", build.Version, "addr", listen, "dicts", len(dicts))

	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()

	reg := prometheus.NewRegistry()
	srv := dictsrv.New(dicts, dictsrv.NewMetrics(reg))
	serveErr := make(chan error, 1)
	go func() { serveErr <- srv.Serve(ctx, ln) }()
	go runTelemetry(cfg.Telemetry, reg, func() bool { return len(dicts) > 0 })

	sigCh := make(chan os.Signal, 1)
	signal.Notify(sigCh, syscall.SIGTERM, syscall.SIGINT)
	select {
	case sig := <-sigCh:
		slog.Info("received signal, shutting down", "signal", sig.String())
		cancel()
	case err := <-serveErr:
		if err != nil {
			slog.Error("serve failed", "err", err)
			os.Exit(1)
		}
	}
}

// openDicts opens every declared dict. A driver that fails to open is fatal
// here and nowhere else: a session cannot fall back to opening it itself.
func openDicts(cfgs map[string]config.DictConfig) (map[string]dict.Dict, error) {
	out := make(map[string]dict.Dict, len(cfgs))
	for name, dc := range cfgs {
		d, err := dict.Open(dict.Config{Driver: dc.Driver, Settings: dc.Settings})
		if err != nil {
			closeDicts(out)
			return nil, err
		}
		slog.Info("dict opened", "name", name, "driver", dc.Driver)
		out[name] = d
	}
	return out, nil
}

func closeDicts(ds map[string]dict.Dict) {
	for name, d := range ds {
		if err := d.Close(); err != nil {
			slog.Warn("dict close failed", "name", name, "err", err)
		}
	}
}

// runTelemetry serves /healthz, /readyz and /metrics beside the dict listener.
func runTelemetry(cfg config.TelemetryConfig, reg *prometheus.Registry, ready func() bool) {
	tel := telemetry.NewWithOptions(telemetry.Options{
		Addr:     telemetry.Addr(cfg.Listen),
		Registry: reg,
		Checks:   []telemetry.Check{telemetry.FuncCheck("dicts", ready)},
		Pprof: telemetry.PprofOptions{
			Enabled:       cfg.PprofEnabled,
			BlockRate:     cfg.PprofBlockProfileRate,
			MutexFraction: cfg.PprofMutexProfileFraction,
		},
	})
	if err := tel.ListenAndServe(context.Background()); err != nil {
		slog.Error("telemetry server failed", "err", err)
	}
}

// buildListener serves plain TCP only where a mesh secures the transport; with
// internal TLS on, the dict service is behind mTLS like every other service.
func buildListener(cfg *config.Config, addr string) (net.Listener, error) {
	if !cfg.InternalTLS.Enabled {
		return net.Listen("tcp", addr)
	}
	tlsCfg, err := mtls.ServerConfig(cfg.InternalTLS.Cert, cfg.InternalTLS.Key, cfg.InternalTLS.CA)
	if err != nil {
		return nil, fmt.Errorf("mtls config: %w", err)
	}
	return tls.Listen("tcp", addr, tlsCfg)
}
