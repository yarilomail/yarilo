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

	"github.com/yarilomail/yarilo/v2/internal/jmap"
	"github.com/yarilomail/yarilo/v2/internal/storage/index/file"
	"github.com/yarilomail/yarilo/v2/internal/storage/mailbox/maildir"
	"github.com/yarilomail/yarilo/v2/pkg/jmapcore"
	"github.com/yarilomail/yarilo/v2/pkg/locks"
	"github.com/yarilomail/yarilo/v2/pkg/mailbox"
)

// realJMAP serves the backend JMAP handler behind a stand-in for the login
// layer, so the probe meets the server's own refusals rather than canned ones.
func realJMAP(t *testing.T) {
	t.Helper()
	dir, err := os.MkdirTemp("", "rt")
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = os.RemoveAll(dir) })
	sock := filepath.Join(dir, "l.sock")
	srv := locks.NewServer(locks.NewMemoryBackend(), slog.New(slog.DiscardHandler), nil)
	ln, err := locks.ListenUnix(sock)
	if err != nil {
		t.Fatal(err)
	}
	ctx, cancel := context.WithCancel(context.Background())
	go func() { _ = srv.Serve(ctx, ln) }()
	locker, err := locks.NewClient(context.Background(), locks.DialUnix(sock))
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = locker.Close(); srv.Close(); cancel() })

	const user = "u1@example.com"
	info := &mailbox.UserInfo{Username: user, Home: t.TempDir(), Separator: "/"}
	if err := maildir.New().OpenUser(info).Init(); err != nil {
		t.Fatal(err)
	}
	_, loopback, _ := net.ParseCIDR("127.0.0.0/8")
	backend := jmap.New(jmap.Options{
		Trust: jmap.ResolveTrust(false, true, []*net.IPNet{loopback}),
		Limits: jmapcore.Limits{
			BaseURL: "https://mail.example.com", MaxSizeUpload: 1 << 20, MaxSizeRequest: 1 << 20,
			MaxConcurrentRequests: 4, MaxCallsInRequest: 16, MaxObjectsInGet: 500, MaxObjectsInSet: 500,
		},
		UploadTTL: time.Hour,
		Storage: &jmap.Storage{
			Mailbox:     maildir.New(),
			Index:       file.New(file.WithLocker(locker)),
			ResolveUser: func(string) (*mailbox.UserInfo, error) { return info, nil },
			Locker:      locker,
		},
	}).Handler()
	// The login layer turns Basic credentials into the identity headers (INTERNALS §6).
	login := http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if u, _, ok := r.BasicAuth(); !ok || u != user {
			http.Error(w, "unauthorized", http.StatusUnauthorized)
			return
		}
		r.Header.Del("Authorization")
		r.Header.Set("X-Yarilo-User", user)
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
	setFlag(t, flagJMAPHost, parsed.Hostname())
	setFlag(t, flagJMAPPort, parsed.Port())
	setFlag(t, flagInsecure, true)
	setFlag(t, flagJMAPUser, user)
	setFlag(t, flagJMAPPass, "pw")
}

func TestJMAPRoundTripAgainstTheServer(t *testing.T) {
	realJMAP(t)
	if err := checkJMAPRoundTrip(); err != nil {
		t.Fatal(err)
	}
}
