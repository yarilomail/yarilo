// Command yarilo-master supervises the components of the FreeBSD port: it
// starts them in order, starts again one that dies, and stops them in
// reverse (#2715). Kubernetes and the Linux packages do not run it.
//
//	yarilo-master [run]          supervise, as root
//	yarilo-master check          validate the master section and exit
//	yarilo-master log            the log process, which the supervisor starts
//	yarilo-master ctl <command>  status | stop|start|restart <name>… | reopen
package main

import (
	"context"
	"fmt"
	"io"
	"log/slog"
	"os"
	"os/signal"
	"runtime"
	"strconv"
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
	os.Exit(realMain(os.Args[1:], os.Stdout, os.Stderr))
}

// realMain runs one command. check and ctl answer on stdout and log on
// stderr: a script reads their answer, and a config warning in it would be a
// line of JSON where a status field was expected.
func realMain(args []string, stdout, stderr io.Writer) int {
	cmd := "run"
	if len(args) > 0 {
		cmd = args[0]
	}
	cfgPath := os.Getenv("CONFIG")
	if cfgPath == "" {
		cfgPath = "/usr/local/etc/yarilo/yarilo.yaml"
	}
	switch cmd {
	case "run":
		logging.Setup("master")
		return run(cfgPath)
	case "log":
		logTo(stderr)
		return logRole(cfgPath)
	case "check":
		logTo(stderr)
		return check(cfgPath, stdout, stderr)
	case "ctl":
		logTo(stderr)
		return ctl(cfgPath, args[1:], stdout, stderr)
	default:
		_, _ = fmt.Fprintf(stderr, "usage: yarilo-master [run | check | log | ctl <command>]\n")
		return 2
	}
}

func logTo(w io.Writer) {
	slog.SetDefault(slog.New(slog.NewJSONHandler(w, &slog.HandlerOptions{
		Level: logging.ParseLevel(os.Getenv("LOG_LEVEL")),
	})).With("service", "master"))
}

func plan(cfgPath string) (*master.Plan, *config.Config, error) {
	cfg, err := config.Load(cfgPath)
	if err != nil {
		return nil, nil, fmt.Errorf("load %s: %w", cfgPath, err)
	}
	p, err := master.DefaultChecker().Build(cfg, cfgPath)
	return p, cfg, err
}

func check(cfgPath string, stdout, stderr io.Writer) int {
	p, _, err := plan(cfgPath)
	if err != nil {
		_, _ = fmt.Fprintf(stderr, "yarilo-master: %v\n", err)
		return 1
	}
	_, _ = fmt.Fprintln(stdout, checkLine(p))
	return 0
}

// checkLine is check's answer: the configuration is good, and the GOMAXPROCS
// the components will get, with where it comes from.
func checkLine(p *master.Plan) string {
	return fmt.Sprintf("yarilo-master: configuration OK; components get GOMAXPROCS=%s (%s)", p.GOMAXPROCS, p.GOMAXPROCSFrom)
}

// setGOMAXPROCS applies to the supervisor the value the components get:
// master_environment's, else 2. With every core the Go scheduler spun on the
// FreeBSD host, 252 % of CPU against 25 % at 2 (#2715).
func setGOMAXPROCS(v string) {
	if n, err := strconv.Atoi(v); err == nil && n > 0 {
		runtime.GOMAXPROCS(n)
	}
}

// logRole is the log process: it reads the pipes the supervisor handed it and
// writes each component's lines to the configured destination, as the log
// account. Its counters are on its own telemetry port.
func logRole(cfgPath string) int {
	cfg, err := config.Load(cfgPath)
	if err != nil {
		slog.Error("log: load config", "err", err)
		return 1
	}
	setGOMAXPROCS(os.Getenv("GOMAXPROCS"))
	reg := prometheus.NewRegistry()
	reg.MustRegister(collectors.NewProcessCollector(collectors.ProcessCollectorOpts{}), collectors.NewGoCollector())
	tel := telemetry.NewWithOptions(telemetry.Options{
		Addr:     telemetry.Addr(""),
		Registry: reg,
		Pprof: telemetry.PprofOptions{
			Enabled:       cfg.Telemetry.PprofEnabled,
			BlockRate:     cfg.Telemetry.PprofBlockProfileRate,
			MutexFraction: cfg.Telemetry.PprofMutexProfileFraction,
		},
	})
	if err := tel.Start(context.Background()); err != nil {
		slog.Error("log: telemetry listen failed", "err", err)
		return 1
	}
	sigs := make(chan os.Signal, 4)
	signal.Notify(sigs, syscall.SIGTERM, syscall.SIGINT, syscall.SIGHUP)
	target := master.LogTarget{Path: cfg.Master.LogPath, Facility: cfg.Master.SyslogFacility}
	if err := master.RunLog(target, os.Getenv(master.LogFDsEnv), reg, sigs, slog.Default()); err != nil {
		slog.Error("log: stopped", "err", err)
		return 1
	}
	return 0
}

func ctl(cfgPath string, args []string, stdout, stderr io.Writer) int {
	if len(args) == 0 {
		_, _ = fmt.Fprintf(stderr, "usage: yarilo-master ctl status [name…] | stop|start|restart <name>… | reopen\n")
		return 2
	}
	cfg, err := config.Load(cfgPath)
	if err != nil {
		_, _ = fmt.Fprintf(stderr, "yarilo-master: load %s: %v\n", cfgPath, err)
		return 1
	}
	ctx, cancel := context.WithTimeout(context.Background(), 15*time.Minute)
	defer cancel()
	if err := master.Ctl(ctx, master.ControlSocket(cfg.Master.RunDir), args, stdout); err != nil {
		_, _ = fmt.Fprintf(stderr, "yarilo-master: %v\n", err)
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
	setGOMAXPROCS(p.GOMAXPROCS)

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
	slog.SetDefault(slog.New(slog.NewJSONHandler(sup.OwnLog(os.Stderr), nil)).With("service", "master"))

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
	slog.Info("master: components get GOMAXPROCS", "value", p.GOMAXPROCS, "from", p.GOMAXPROCSFrom)
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
		return 0
	}
	return 0
}
