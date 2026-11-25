// Command yarilo-master supervises the components of the FreeBSD port: it
// starts them in order, starts again one that dies, and stops them in
// reverse (#2715). Kubernetes and the Linux packages do not run it.
//
//	yarilo-master [run]          supervise, as root
//	yarilo-master check          validate the master section and exit
//	yarilo-master ctl <command>  status | stop|start|restart <name>… | reopen
package main

import (
	"context"
	"fmt"
	"log/slog"
	"os"
	"os/signal"
	"syscall"
	"time"

	"github.com/prometheus/client_golang/prometheus"
	"github.com/prometheus/client_golang/prometheus/collectors"

	"github.com/yarilomail/yarilo/v2/internal/master"
	"github.com/yarilomail/yarilo/v2/internal/telemetry"
	"github.com/yarilomail/yarilo/v2/pkg/config"
	"github.com/yarilomail/yarilo/v2/pkg/logging"
)

func main() {
	logging.Setup("master")
	cmd := "run"
	if len(os.Args) > 1 {
		cmd = os.Args[1]
	}
	cfgPath := os.Getenv("CONFIG")
	if cfgPath == "" {
		cfgPath = "/usr/local/etc/yarilo/yarilo.yaml"
	}
	switch cmd {
	case "run":
		os.Exit(run(cfgPath))
	case "check":
		os.Exit(check(cfgPath))
	case "ctl":
		os.Exit(ctl(cfgPath, os.Args[2:]))
	default:
		fmt.Fprintf(os.Stderr, "usage: yarilo-master [run | check | ctl <command>]\n")
		os.Exit(2)
	}
}

func plan(cfgPath string) (*master.Plan, *config.Config, error) {
	cfg, err := config.Load(cfgPath)
	if err != nil {
		return nil, nil, fmt.Errorf("load %s: %w", cfgPath, err)
	}
	p, err := master.DefaultChecker().Build(cfg, cfgPath)
	return p, cfg, err
}

func check(cfgPath string) int {
	if _, _, err := plan(cfgPath); err != nil {
		fmt.Fprintf(os.Stderr, "yarilo-master: %v\n", err)
		return 1
	}
	fmt.Println("yarilo-master: configuration OK")
	return 0
}

func ctl(cfgPath string, args []string) int {
	if len(args) == 0 {
		fmt.Fprintf(os.Stderr, "usage: yarilo-master ctl status | stop|start|restart <name>… | reopen\n")
		return 2
	}
	cfg, err := config.Load(cfgPath)
	if err != nil {
		fmt.Fprintf(os.Stderr, "yarilo-master: load %s: %v\n", cfgPath, err)
		return 1
	}
	ctx, cancel := context.WithTimeout(context.Background(), 15*time.Minute)
	defer cancel()
	if err := master.Ctl(ctx, master.ControlSocket(cfg.Master.RunDir), args, os.Stdout); err != nil {
		fmt.Fprintf(os.Stderr, "yarilo-master: %v\n", err)
		return 1
	}
	return 0
}

func run(cfgPath string) int {
	if os.Geteuid() != 0 {
		slog.Error("master: runs as root, to start each component as its own user")
		return 1
	}
	p, cfg, err := plan(cfgPath)
	if err != nil {
		slog.Error("master: configuration refused; nothing started", "err", err)
		return 1
	}
	// Inherited by every component; it was daemon's and the login class's.
	syscall.Umask(0o027)

	reg := prometheus.NewRegistry()
	reg.MustRegister(collectors.NewProcessCollector(collectors.ProcessCollectorOpts{}), collectors.NewGoCollector())
	sup, err := master.New(p, master.Options{
		Logger:     slog.Default(),
		Registerer: reg,
		AsRoot:     true,
		PreStart:   master.HostPreStart,
		IsOurs:     master.IsOurs,
	})
	if err != nil {
		slog.Error("master: could not prepare", "err", err)
		return 1
	}
	own, err := sup.OwnLog()
	if err != nil {
		slog.Error("master: could not open its own log", "err", err)
		return 1
	}
	slog.SetDefault(slog.New(slog.NewJSONHandler(own, nil)).With("service", "master"))

	tel := telemetry.NewWithOptions(telemetry.Options{
		Addr:     telemetry.Addr(p.Telemetry),
		Registry: reg,
		Pprof: telemetry.PprofOptions{
			Enabled:       cfg.Telemetry.PprofEnabled,
			BlockRate:     cfg.Telemetry.PprofBlockProfileRate,
			MutexFraction: cfg.Telemetry.PprofMutexProfileFraction,
		},
	})
	// Fatal, as for every component (#2419): the restart counts live here,
	// and a supervisor nobody can read is blind.
	if err := tel.Start(context.Background()); err != nil {
		slog.Error("master: telemetry listen failed; nothing started", "addr", p.Telemetry, "err", err)
		sup.Stop()
		sup.CloseOwnLog()
		return 1
	}

	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	go func() {
		if err := sup.Serve(ctx, master.ControlSocket(p.RunDir), 0); err != nil {
			slog.Error("master: control socket", "err", err)
		}
	}()

	sigs := make(chan os.Signal, 4)
	signal.Notify(sigs, syscall.SIGTERM, syscall.SIGINT, syscall.SIGHUP)
	slog.Info("master: starting", "components", len(p.Components), "config", cfgPath)
	go sup.Run()
	for sig := range sigs {
		if sig == syscall.SIGHUP {
			if err := sup.Reopen(); err != nil {
				slog.Error("master: reopen", "err", err)
			}
			continue
		}
		slog.Info("master: stopping", "signal", sig.String())
		cancel()
		sup.Stop()
		slog.Info("master: stopped")
		sup.CloseOwnLog()
		return 0
	}
	return 0
}
