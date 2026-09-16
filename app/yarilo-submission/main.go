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

	"github.com/emersion/go-sasl"

	"github.com/yarilomail/yarilo/internal/auth/passdbs"
	"github.com/yarilomail/yarilo/internal/auth/protocol"
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

	// The shared builder: a loop of its own handed every driver to the SQL
	// constructor and killed the process at start (#1861).
	dbs, _, err := passdbs.Build(cfg.Auth.Passdb)
	if err != nil {
		slog.Error("passdb init failed", "err", err)
		os.Exit(1)
	}

	authCache := protocol.NewCache(
		cfg.Auth.Cache.CacheSizeBytes(),
		time.Duration(cfg.Auth.Cache.TTLSeconds)*time.Second,
		time.Duration(cfg.Auth.Cache.NegativeTTLSeconds)*time.Second,
	)
	authOpts := []protocol.AuthenticatorOption{
		protocol.WithAuthenticatorCache(authCache),
	}
	if cfg.Auth.MasterUsers.Enabled {
		masterdbs, _, merr := passdbs.Build(cfg.Auth.MasterUsers.Masterdb)
		if merr != nil {
			slog.Error("masterdb init failed", "err", merr)
			os.Exit(1)
		}
		authOpts = append(authOpts,
			protocol.WithAuthenticatorMasterUsers(true),
			protocol.WithAuthenticatorMasterdb(masterdbs),
			protocol.WithAuthenticatorMasterUserSeparator(cfg.Auth.MasterUsers.Separator),
		)
	}

	// ---- relay proxy ----
	var relay *submproxy.Submission
	if cfg.Protocol.Submission.Relay.Host != "" {
		relay = submproxy.New(cfg.Protocol.Submission.Relay, cfg.SubmissionHostname())
	}

	// ---- TLS ----
	var extTLS *tls.Config
	if cfg.General.SSL.SSLServerCert != "" && cfg.General.SSL.SSLServerKey != "" {
		extTLS, err = config.BuildTLSConfig(cfg.General.SSL)
		if err != nil {
			slog.Error("TLS config failed", "err", err)
			os.Exit(1)
		}
		extTLS.NextProtos = []string{"smtp"}
	}

	haproxyNets := parseCIDRs(cfg.General.HAProxy.HAProxyTrustedNetworks)
	haproxyTimeout := time.Duration(cfg.General.HAProxy.Timeout) * time.Second

	authAddr := cfg.AuthService.ClientAddr()
	var authTLS *tls.Config
	if cfg.InternalTLS.Enabled {
		t, err := mtls.ClientConfig(cfg.InternalTLS.Cert, cfg.InternalTLS.Key, cfg.InternalTLS.CA, cfg.InternalTLS.ServerName, cfg.InternalTLS.SessionCacheSize, cfg.InternalTLS.SessionCacheTTL)
		if err != nil {
			slog.Error("auth_service mtls config failed", "err", err)
			os.Exit(1)
		}
		authTLS = t
	}

	primary := firstActive(svcs.Submission, svcs.Submissions)
	srv := submsvr.New(submsvr.Options{
		HAProxy:          primary.HAProxy,
		HAProxyTimeout:   haproxyTimeout,
		HAProxyNets:      haproxyNets,
		AuthAddr:         authAddr,
		AuthTLS:          authTLS,
		DisablePlainAuth: primary.PlainAuthDisabled(),
		TLSConfig:        extTLS,
		Config:           cfg.Protocol.Submission,
		Auth:             chainAuth{protocol.NewAuthenticator(dbs, authOpts...)},
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

// chainAuth adapts protocol.Authenticator to submission.Authenticator: the
// wire surface takes (username, password) and wants only the verdict.
type chainAuth struct{ c protocol.Authenticator }

func (a chainAuth) AuthPlain(username, password string) error {
	resp, err := a.c.Authenticate(username, password, "smtp", "")
	if err != nil {
		return fmt.Errorf("smtp/auth: %w", err)
	}
	if resp == nil || resp.Result != protocol.AuthOK {
		return fmt.Errorf("smtp/auth: authentication failed")
	}
	return nil
}

// AuthPlainMaster takes a non-empty authzid through the master-user flow; a
// chain without it fails opaquely, so the wire reply matches a wrong password.
func (a chainAuth) AuthPlainMaster(authzid, authid, password string) error {
	master, ok := a.c.(protocol.MasterAuthenticator)
	if !ok {
		return fmt.Errorf("smtp/auth: authentication failed")
	}
	resp, err := master.AuthenticateMaster(authzid, authid, password, "smtp", "")
	if err != nil {
		return fmt.Errorf("smtp/auth: %w", err)
	}
	if resp == nil || resp.Result != protocol.AuthOK {
		return fmt.Errorf("smtp/auth: authentication failed")
	}
	return nil
}

// LookupSCRAMSha256 forwards to the chain; (nil, nil) from one without SCRAM
// is what keeps the mechanism out of EHLO.
func (a chainAuth) LookupSCRAMSha256(username string) (*sasl.ScramCredentials, error) {
	lookup, ok := a.c.(protocol.SCRAMSha256Lookup)
	if !ok {
		return nil, nil
	}
	return lookup.LookupSCRAMSha256(username)
}

// LookupSCRAMSha1 is the SHA-1 counterpart of LookupSCRAMSha256.
func (a chainAuth) LookupSCRAMSha1(username string) (*sasl.ScramCredentials, error) {
	lookup, ok := a.c.(protocol.SCRAMSha1Lookup)
	if !ok {
		return nil, nil
	}
	return lookup.LookupSCRAMSha1(username)
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
