// yarilo-submission is the SMTP submission proxy: it authenticates the client
// on 587/465 and relays to the upstream MTA, opening no mailbox.
package main

import (
	"context"
	"crypto/tls"
	"fmt"
	"log/slog"
	"net"
	"os"
	"os/signal"
	"sync/atomic"
	"syscall"
	"time"

	authrelay "github.com/yarilomail/yarilo/internal/auth/client"

	"github.com/yarilomail/yarilo/internal/readyfile"
	submsvr "github.com/yarilomail/yarilo/internal/submission"
	submproxy "github.com/yarilomail/yarilo/internal/submission/proxy"
	"github.com/yarilomail/yarilo/internal/telemetry"
	"github.com/yarilomail/yarilo/pkg/build"
	"github.com/yarilomail/yarilo/pkg/config"
	"github.com/yarilomail/yarilo/pkg/logging"
	"github.com/yarilomail/yarilo/pkg/mtls"
)

// version is set via pkg/build; kept for vet compatibility

func main() {
	logging.Setup("submission")

	cfgPath := os.Getenv("CONFIG")
	if cfgPath == "" {
		cfgPath = "/etc/yarilo/yarilo.yaml"
	}
	cfg, err := config.Load(cfgPath)
	if err != nil {
		slog.Error("config load failed", "err", err, "path", cfgPath)
		os.Exit(1)
	}

	svcs := cfg.Services
	if !svcs.Submission.Active() && !svcs.Submissions.Active() {
		slog.Error("no submission listener configured (submission or submissions must be enabled)")
		os.Exit(1)
	}

	slog.Info("yarilo-submission starting",
		"version", build.Version,
		"telemetry", telemetry.Addr(cfg.Telemetry.Listen), // resolved (honours TELEMETRY_LISTEN)
	)

	// One listener per session binary; the login proxy holds the client
	// certificate this process must not read (#1863).
	config.KeepOnlySessionListener(cfg, config.RoleSubmission)

	// ---- relay proxy ----
	var relay *submproxy.Submission
	if cfg.Protocol.Submission.Relay.Host != "" {
		relay = submproxy.New(cfg.Protocol.Submission.Relay, cfg.SubmissionHostname())
	}

	// ---- TLS ----
	// Only for the listener that terminates it: behind submission-login the
	// certificate lives in the login pod and this path does not exist (#1863).
	extTLS, err := config.ListenerTLS(cfg, cfg.Services.Submissions, "smtp")
	if err != nil {
		slog.Error("TLS config failed", "err", err)
		os.Exit(1)
	}

	haproxyNets := parseCIDRs(cfg.General.HAProxy.HAProxyTrustedNetworks)
	haproxyTimeout := time.Duration(cfg.General.HAProxy.Timeout) * time.Second

	authAddr := cfg.AuthService.ClientAddr()
	if authAddr == "" {
		slog.Error("submission: cannot start", "err", authrelay.ErrNoAuthService)
		os.Exit(1)
	}
	var authTLS *tls.Config
	if cfg.InternalTLS.Enabled {
		t, err := mtls.ClientConfig(cfg.InternalTLS.Cert, cfg.InternalTLS.Key, cfg.InternalTLS.CA, cfg.InternalTLS.ServerName, cfg.InternalTLS.SessionCacheSize, cfg.InternalTLS.SessionCacheTTL)
		if err != nil {
			slog.Error("auth_service mtls config failed", "err", err)
			os.Exit(1)
		}
		authTLS = t
	}

	// Direct clients on 587 reach this listener without a login proxy, so this
	// process needs its own relay to the auth service (#1733).
	authRelay, err := authrelay.Dial(authAddr, authTLS)
	if err != nil {
		slog.Error("submission: auth relay", "addr", authAddr, "err", err)
		os.Exit(1)
	}

	primary := firstActive(svcs.Submission, svcs.Submissions)
	srv := submsvr.New(submsvr.Options{
		HAProxy:          primary.HAProxy,
		HAProxyTimeout:   haproxyTimeout,
		HAProxyNets:      haproxyNets,
		AuthAddr:         authAddr,
		AuthRelay:        authRelay,
		AuthTLS:          authTLS,
		DisablePlainAuth: primary.PlainAuthDisabled(),
		TLSConfig:        extTLS,
		Config:           cfg.Protocol.Submission,
		Proxy:            relay,
		FailureDelay:     time.Duration(cfg.Auth.FailureDelaySeconds) * time.Second,
		OAuth2Enabled:    len(cfg.Auth.OAuth2) > 0,
	})

	go runTelemetry(cfg.Telemetry)

	// The sidecar gates the pod's director heartbeat on this file (#788); a
	// relay proxy is ready once its listeners are bound.
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	var ready atomic.Bool
	reg := cfg.BackendRegister
	go readyfile.Touch(ctx, reg.ReadinessDir, "submission",
		time.Duration(reg.ReadinessTouchInterval)*time.Second, ready.Load)

	// port 587 — STARTTLS
	if svcs.Submission.Active() {
		addr := fmt.Sprintf(":%d", svcs.Submission.Port)
		ln, err := net.Listen("tcp", addr)
		if err != nil {
			slog.Error("submission: listen failed", "addr", addr, "err", err)
			os.Exit(1)
		}
		go func() {
			if err := srv.Serve(ln, nil); err != nil {
				slog.Error("submission: server error", "err", err)
				os.Exit(1)
			}
		}()
		slog.Info("submission: listening", "addr", addr, "tls", "starttls")
	}

	// port 465 — implicit TLS
	if svcs.Submissions.Active() {
		addr := fmt.Sprintf(":%d", svcs.Submissions.Port)
		ln, err := net.Listen("tcp", addr)
		if err != nil {
			slog.Error("submissions: listen failed", "addr", addr, "err", err)
			os.Exit(1)
		}
		go func() {
			if err := srv.Serve(ln, extTLS); err != nil {
				slog.Error("submissions: server error", "err", err)
				os.Exit(1)
			}
		}()
		slog.Info("submission: listening", "addr", addr, "tls", "implicit")
	}

	ready.Store(true) // listeners bound → the readiness toucher may start signalling

	sigCh := make(chan os.Signal, 1)
	signal.Notify(sigCh, syscall.SIGTERM, syscall.SIGINT)
	sig := <-sigCh
	slog.Info("received signal, shutting down", "signal", sig.String())
	cancel() // stop touching the readiness file so the sidecar drops this pod
	slog.Info("yarilo-submission stopped")
}

func parseCIDRs(ss []string) []*net.IPNet {
	nets := make([]*net.IPNet, 0, len(ss))
	for _, s := range ss {
		_, n, err := net.ParseCIDR(s)
		if err != nil {
			slog.Warn("submission: invalid CIDR", "cidr", s, "err", err)
			continue
		}
		nets = append(nets, n)
	}
	return nets
}

func firstActive(svcs ...*config.ServiceConfig) *config.ServiceConfig {
	for _, s := range svcs {
		if s != nil && s.Enabled {
			return s
		}
	}
	return &config.ServiceConfig{}
}

func runTelemetry(cfg config.TelemetryConfig) {
	// No Checks yet: /readyz answered an unconditional 200 before this was
	// shared, and giving it a real condition is a behaviour change.
	tel := telemetry.NewWithOptions(telemetry.Options{
		Addr: telemetry.Addr(cfg.Listen),
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
