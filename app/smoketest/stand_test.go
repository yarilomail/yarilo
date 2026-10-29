package main

import (
	"context"
	"log/slog"
	"net"
	"net/http"
	"net/http/httptest"
	"net/url"
	"os"
	"path/filepath"
	"testing"
	"time"

	"github.com/yarilomail/yarilo/v2/internal/auth/authtest"
	"github.com/yarilomail/yarilo/v2/internal/auth/protocol"
	"github.com/yarilomail/yarilo/v2/internal/backendapi"
	imapserver "github.com/yarilomail/yarilo/v2/internal/imap"
	"github.com/yarilomail/yarilo/v2/internal/jmap"
	"github.com/yarilomail/yarilo/v2/internal/lmtp"
	"github.com/yarilomail/yarilo/v2/internal/pop3"
	"github.com/yarilomail/yarilo/v2/internal/storage/index/file"
	"github.com/yarilomail/yarilo/v2/internal/storage/mailbox/maildir"
	"github.com/yarilomail/yarilo/v2/pkg/authclient"
	"github.com/yarilomail/yarilo/v2/pkg/config"
	"github.com/yarilomail/yarilo/v2/pkg/ftsproto"
	"github.com/yarilomail/yarilo/v2/pkg/jmapcore"
	"github.com/yarilomail/yarilo/v2/pkg/locks"
	"github.com/yarilomail/yarilo/v2/pkg/mailbox"
	"github.com/yarilomail/yarilo/v2/pkg/quota"
)

// The account every stand row reads through each surface.
const (
	standUser = "u1@example.com"
	standPass = "pw"
)

// localStand is yarilo's own servers on loopback over one maildir and file
// index, with the real lock service: the probe meets the server, not a stub.
type localStand struct {
	root     string
	resolver *mailbox.Resolver
	locker   locks.Locker
	lmtpAddr string
	relayCfg *config.RelayConfig
}

// standQuotaRule is the user's limit, from userdb as production has it: the
// quota rows need a root to read, and without a limit there is none.
const standQuotaRule = "*:storage=100M"

type standPassdb struct{}

func (standPassdb) Authenticate(req *protocol.Request) (protocol.Result, error) {
	if req.Username != standUser || req.Password != standPass {
		return protocol.ResultNext, nil
	}
	req.Fields.Set("quota_rule", standQuotaRule)
	return protocol.ResultOK, nil
}

func startStand(t *testing.T) *localStand {
	t.Helper()
	dir, err := os.MkdirTemp("", "st")
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = os.RemoveAll(dir) })
	sock := filepath.Join(dir, "l.sock")
	lsrv := locks.NewServer(locks.NewMemoryBackend(), slog.New(slog.DiscardHandler), nil)
	lln, err := locks.ListenUnix(sock)
	if err != nil {
		t.Fatal(err)
	}
	ctx, cancel := context.WithCancel(context.Background())
	go func() { _ = lsrv.Serve(ctx, lln) }()
	locker, err := locks.NewClient(context.Background(), locks.DialUnix(sock))
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = locker.Close(); lsrv.Close(); cancel() })

	s := &localStand{root: t.TempDir(), locker: locker}
	// A limit, so the quota rows have a root to read: without one there is none.
	s.resolver = &mailbox.Resolver{Root: s.root, HomeTemplate: "%d/%n", DefaultQuotaRules: []string{"*:storage=100M"}}
	info, err := s.resolver.UserInfo(standUser, "")
	if err != nil {
		t.Fatal(err)
	}
	if err := maildir.New().OpenUser(info).Init(); err != nil {
		t.Fatal(err)
	}
	return s
}

func (s *localStand) listen(t *testing.T) (net.Listener, string) {
	t.Helper()
	ln, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = ln.Close() })
	_, port, _ := net.SplitHostPort(ln.Addr().String())
	return ln, port
}

// withIMAP serves IMAP in the clear and points the probe's IMAP flags at it.
func (s *localStand) withIMAP(t *testing.T, tune ...func(*imapserver.Options)) {
	t.Helper()
	opts := imapserver.Options{
		Mailbox:     maildir.New(),
		Index:       file.New(file.WithLocker(s.locker)),
		Resolver:    s.resolver,
		AuthRelay:   authtest.RelayToChain(t, standPassdb{}),
		Locker:      s.locker,
		ACLEnabled:  true,
		IMAPQuota:   true,
		QuotaEngine: true,
		Namespaces: []imapserver.NamespaceSpec{
			{Type: imapserver.NamespacePersonal, Prefix: "", Separator: '/', List: imapserver.ListYes},
		},
	}
	opts.QuotaPolicy = quotaPolicyAll()
	for _, f := range tune {
		f(&opts)
	}
	srv := imapserver.New(opts)
	ln, port := s.listen(t)
	go func() { _ = srv.Serve(ln) }()
	setFlag(t, flagIMAPHost, "127.0.0.1")
	setFlag(t, flagIMAPSPort, port)
	setFlag(t, flagIMAPTLS, "none")
	setFlag(t, flagIMAPUser, standUser)
	setFlag(t, flagIMAPPass, standPass)
}

// withLMTP serves delivery over LMTP and points the probe's injection at it.
func (s *localStand) withLMTP(t *testing.T, tune ...func(*lmtp.Options)) {
	t.Helper()
	opts := lmtp.Options{
		Hostname: "lmtp.test",
		Config: config.LMTPProtocolConfig{AddReceivedHeader: true, AddMessageID: true, HdrDeliveryAddress: "final",
			ReadTimeout: 5, WriteTimeout: 5},
		Mailbox:  maildir.New(),
		Index:    file.New(file.WithLocker(s.locker)),
		Resolver: s.resolver,
		Locker:   s.locker,
	}
	opts.QuotaPolicy.StoragePercentage, opts.QuotaPolicy.MessagePercentage = 100, 100
	for _, f := range tune {
		f(&opts)
	}
	srv := lmtp.New(opts)
	ln, port := s.listen(t)
	go func() { _ = srv.Serve(ln) }()
	s.lmtpAddr = ln.Addr().String()
	setFlag(t, flagDeliveryHost, "127.0.0.1")
	setFlag(t, flagDeliveryPort, port)
	setFlag(t, flagDeliveryProto, "lmtp")
}

// withJMAP serves the JMAP backend behind a stand-in for the login layer, which
// turns Basic credentials into the identity headers (INTERNALS §6).
func (s *localStand) withJMAP(t *testing.T, tune ...func(*jmap.Options)) {
	t.Helper()
	_, loopback, _ := net.ParseCIDR("127.0.0.0/8")
	opts := jmap.Options{
		Trust: jmap.ResolveTrust(false, true, []*net.IPNet{loopback}),
		Limits: jmapcore.Limits{
			BaseURL: "https://mail.example.com", MaxSizeUpload: 1 << 20, MaxSizeRequest: 1 << 20,
			MaxConcurrentRequests: 4, MaxCallsInRequest: 16, MaxObjectsInGet: 500, MaxObjectsInSet: 500,
		},
		UploadTTL:        time.Hour,
		QuotaEnabled:     true,
		PushPoll:         time.Second,
		WebSocketTimeout: time.Minute,
		Storage: &jmap.Storage{
			Mailbox:     maildir.New(),
			Index:       file.New(file.WithLocker(s.locker)),
			ResolveUser: s.userInfo,
			Locker:      s.locker,
		},
		QuotaPolicy: quotaPolicyAll(),
	}
	for _, f := range tune {
		f(&opts)
	}
	backend := jmap.New(opts).Handler()
	login := http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		u, p, ok := r.BasicAuth()
		if !ok || u != standUser || p != standPass {
			http.Error(w, "unauthorized", http.StatusUnauthorized)
			return
		}
		r.Header.Del("Authorization")
		r.Header.Set("X-Yarilo-User", standUser)
		r.Header.Set("X-Session-ID", "deadbeefdeadbeef")
		r.Header.Set("X-Proxy-TTL", "4")
		backend.ServeHTTP(w, r)
	})
	ts := httptest.NewTLSServer(login)
	t.Cleanup(ts.Close)
	parsed, err := url.Parse(ts.URL)
	if err != nil {
		t.Fatal(err)
	}
	// The probe caches the account id once per process; this server's is its own.
	jmapAccount = &accountCache{}
	t.Cleanup(func() { jmapAccount = &accountCache{} })
	setFlag(t, flagJMAP, true)
	setFlag(t, flagJMAPHost, parsed.Hostname())
	setFlag(t, flagJMAPPort, parsed.Port())
	setFlag(t, flagInsecure, true)
	setFlag(t, flagJMAPUser, standUser)
	setFlag(t, flagJMAPPass, standPass)
}

// quotaPolicyAll is the policy the chart defaults give: limits at 100%.
func quotaPolicyAll() quota.Policy {
	return quota.Policy{StoragePercentage: 100, MessagePercentage: 100}
}

// userInfo is the user as the login path resolves it, quota rule included.
func (s *localStand) userInfo(user string) (*mailbox.UserInfo, error) {
	info, err := s.resolver.UserInfo(user, "")
	if err != nil {
		return nil, err
	}
	info.QuotaRules = []string{standQuotaRule}
	return info, nil
}

// withBackendAPI serves the admin API over the same store and points the
// probe at it; fts may be nil when the stand runs without yarilo-fts.
func (s *localStand) withBackendAPI(t *testing.T, fts ftsproto.Client) {
	t.Helper()
	srv := backendapi.New(backendapi.Options{
		Token:      "stand-token",
		Mailbox:    mailbox.Validating(maildir.New(), mailbox.DefaultNameRules()),
		Index:      file.New(file.WithLocker(s.locker)),
		Resolver:   s.resolver,
		Locker:     s.locker,
		Namespaces: []config.NamespaceConfig{{Type: "personal", Prefix: "", Separator: "/", List: "yes", Inbox: true}},
		FTSClient:  fts,
		AuthClient: s.authMaster(t),
	})
	ts := httptest.NewServer(srv.Handler())
	t.Cleanup(ts.Close)
	setFlag(t, flagBackendAPI, ts.URL)
	setFlag(t, flagBackendAPIToken, "stand-token")
}

type standUserdb struct{}

func (standUserdb) Lookup(user string) (*protocol.UserInfo, error) {
	if user != standUser {
		return nil, nil
	}
	return &protocol.UserInfo{Username: user, QuotaRules: []string{standQuotaRule}}, nil
}

// authMaster is yarilo-auth's master protocol over the stand's userdb, the
// lookup the admin API reads a user's limits from.
func (s *localStand) authMaster(t *testing.T) *authclient.Client {
	t.Helper()
	ln, _ := s.listen(t)
	ctx, cancel := context.WithCancel(context.Background())
	t.Cleanup(cancel)
	go func() { _ = protocol.NewMasterServer(standUserdb{}).Serve(ctx, ln) }()
	c, err := authclient.Dial(ln.Addr().String(), nil)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = c.Close() })
	return c
}

// withPOP3 serves POP3 in the clear and points the probe's POP3 flags at it.
func (s *localStand) withPOP3(t *testing.T) {
	t.Helper()
	srv := pop3.New(pop3.Options{
		Mailbox:   maildir.New(),
		Index:     file.New(file.WithLocker(s.locker)),
		Resolver:  s.resolver,
		AuthRelay: authtest.RelayToChain(t, standPassdb{}),
		Locker:    s.locker,
	})
	ln, port := s.listen(t)
	go func() { _ = srv.Serve(ln) }()
	setFlag(t, flagPOP3Host, "127.0.0.1")
	setFlag(t, flagPOP3Port, port)
	setFlag(t, flagPOP3TLS, "none")
	setFlag(t, flagPOP3User, standUser)
	setFlag(t, flagPOP3Pass, standPass)
}
