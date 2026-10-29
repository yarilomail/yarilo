package main

import (
	"context"
	"net"
	"strconv"
	"testing"
	"time"

	"github.com/yarilomail/yarilo/v2/internal/auth/protocol"
	"github.com/yarilomail/yarilo/v2/internal/jmap"
	"github.com/yarilomail/yarilo/v2/internal/lmtp"
	"github.com/yarilomail/yarilo/v2/internal/login"
	"github.com/yarilomail/yarilo/v2/internal/managesieve"
	"github.com/yarilomail/yarilo/v2/internal/sieve"
	"github.com/yarilomail/yarilo/v2/pkg/authtoken"
	"github.com/yarilomail/yarilo/v2/pkg/config"
)

// authService is yarilo-auth's client protocol with the session-token store a
// login proxy and the backend behind it share; its address serves both.
func (s *localStand) authService(t *testing.T) string {
	t.Helper()
	if s.authAddr != "" {
		return s.authAddr
	}
	ln, _ := s.listen(t)
	ctx, cancel := context.WithCancel(context.Background())
	t.Cleanup(cancel)
	srv := protocol.NewServer([]protocol.Passdb{standPassdb{}}, protocol.WithTokenStore(authtoken.New(time.Minute)))
	go func() { _ = srv.Serve(ctx, ln) }()
	s.authAddr = ln.Addr().String()
	return s.authAddr
}

// withManageSieve is managesieve-login in front of the ManageSieve backend, the
// pair a client meets on :4190, and points the probe at the login side.
func (s *localStand) withManageSieve(t *testing.T) {
	t.Helper()
	auth := s.authService(t)
	backend := managesieve.New(managesieve.Options{
		DefaultName:   config.Defaults().Sieve.DefaultName,
		Locker:        s.locker,
		Resolver:      s.resolver,
		Config:        config.ManageSieveProtocolConfig{MaxLineLength: 65536},
		MaxScriptSize: 1 << 20,
		MaxScripts:    16,
		MaxStorage:    1 << 22,
		AuthAddr:      auth,
	})
	bln, bport := s.listen(t)
	ctx, cancel := context.WithCancel(context.Background())
	t.Cleanup(cancel)
	go func() { _ = backend.ServeManageSieve(ctx, bln) }()
	proxy := login.New(login.Options{
		Protocol:        login.ProtocolManageSieve,
		BackendAddr:     "127.0.0.1:" + bport,
		LocalIP:         "127.0.0.1",
		StarttlsTLS:     standTLS(t),
		AuthAddr:        auth,
		AuthMaxAttempts: 3,
		SieveExtensions: sieveExtensions(),
		ProxyTimeout:    30 * time.Second,
		DialRetries:     1,
	})
	lln, lport := s.listen(t)
	go func() { _ = proxy.Serve(lln) }()
	setFlag(t, flagManageSieve, true)
	setFlag(t, flagManageSieveHost, "127.0.0.1")
	setFlag(t, flagManageSievePort, lport)
	setFlag(t, flagManageSieveTLS, "starttls")
	setFlag(t, flagManageSieveUser, standUser)
	setFlag(t, flagManageSievePass, standPass)
	setFlag(t, flagInsecure, true)
}

// sieveExtensions is what the login advertises with the defaults.
func sieveExtensions() string {
	var c config.SieveConfig
	exts := sieve.EffectiveExtensions(sieve.UserExtensions(c))
	out := ""
	for i, e := range exts {
		if i > 0 {
			out += " "
		}
		out += e
	}
	return out
}

// standSieve is the stand's sieve config: the sandbox's extensions and spamtest
// header, vacation and notify relayed through the MTA stand-in.
func (s *localStand) standSieve(t *testing.T) config.SieveConfig {
	t.Helper()
	c := config.Defaults().Sieve
	c.Enabled, c.VacationEnabled, c.VacationUseOriginalRecipient = true, true, true
	c.MaxActions = 32
	c.SpamStatusHeader = "X-Spam-Score"
	relay := s.relay(t)
	c.SubmissionHost, c.SubmissionSSL, c.SubmissionTimeout = net.JoinHostPort(relay.Host, strconv.Itoa(relay.Port)), "no", 5
	c.SieveExtensions = []string{"fileinto", "reject", "ereject", "envelope", "encoded-character", "variables",
		"relational", "subaddress", "body", "date", "index", "mailbox", "mailboxid", "mboxmetadata",
		"servermetadata", "duplicate", "imap4flags", "redirect", "copy", "vacation", "include", "enotify",
		"spamtest", "spamtestplus", "virustest", "comparator-i;ascii-numeric", "foreverypart", "mime",
		"extracttext", "replace", "enclose", "vnd.yarilo.debug", "vnd.yarilo.filter", "vnd.yarilo.execute",
		"vnd.yarilo.pipe", "vnd.yarilo.report", "environment", "vnd.yarilo.environment"}
	return c
}

// withSieve is the stand delivering through the sieve engine, with
// ManageSieve in front of the scripts it runs.
func (s *localStand) withSieve(t *testing.T) {
	t.Helper()
	s.withIMAP(t)
	// One METADATA store for both, as the deployment shares it: a script reads
	// what IMAP SETMETADATA wrote.
	s.withLMTP(t, func(o *lmtp.Options) {
		o.SieveEngine = sieve.New(s.standSieve(t), s.locker, nil, nil)
		o.MetadataDict = s.metadata
	})
	s.withManageSieve(t)
	cfg := s.standSieve(t)
	s.withJMAP(t, func(o *jmap.Options) {
		o.Sieve = &jmap.SieveScripts{
			Store:          sieve.NewScriptStore(cfg.ScriptsDriver, cfg.DefaultName, s.locker, nil),
			Quota:          sieve.Quota{MaxSize: cfg.MaxScriptSize, MaxScripts: cfg.MaxScripts, MaxStorage: cfg.MaxStorage},
			Extensions:     sieve.UserExtensions(cfg),
			MaxRedirects:   cfg.MaxRedirects,
			Implementation: "yarilo stand",
			Vacation:       true,
		}
	})
	setFlag(t, flagSieve, true)
}

// The ManageSieve rows through the login proxy to the backend.
func TestManageSieveRowsAgainstTheServers(t *testing.T) {
	s := startStand(t)
	s.withManageSieve(t)
	if err := checkManageSieve(); err != nil {
		t.Fatalf("managesieve auth+CRUD: %v", err)
	}
}

// The sieve rows: each extension runs at delivery and IMAP sees the effect, and
// a script managesieve activates takes effect on the next delivery.
func TestSieveRowsAgainstTheServers(t *testing.T) {
	s := startStand(t)
	s.withSieve(t)
	if err := checkSieve(); err != nil {
		t.Fatalf("sieve plugins: %v", err)
	}
	if err := checkConsistencySieve(standUser, standPass); err != nil {
		t.Fatalf("managesieve script takes effect: %v", err)
	}
	if err := checkConsistencyJMAPSieve(standUser, standPass); err != nil {
		t.Fatalf("jmap SieveScript is managesieve's active script: %v", err)
	}
	if err := checkConsistencyJMAPVacation(standUser, standPass); err != nil {
		t.Fatalf("jmap VacationResponse is managesieve's active script: %v", err)
	}
}
