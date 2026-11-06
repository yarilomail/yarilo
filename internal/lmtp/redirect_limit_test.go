package lmtp

import (
	"context"
	"net"
	"testing"

	goSmtp "github.com/emersion/go-smtp"

	"github.com/yarilomail/yarilo/v2/internal/sieve"
	fileindex "github.com/yarilomail/yarilo/v2/internal/storage/index/file"
	"github.com/yarilomail/yarilo/v2/internal/storage/mailbox/maildir"
	"github.com/yarilomail/yarilo/v2/pkg/config"
	"github.com/yarilomail/yarilo/v2/pkg/mailbox"
)

// One redirect over sieve_max_redirects fails the script: nothing is sent and
// the message falls back to implicit keep in INBOX, discard or not (#2529).
func TestRedirectOverTheLimitKeepsTheMessage(t *testing.T) {
	cases := []struct {
		name      string
		script    string
		wantSent  string
		wantInbox int
	}{
		{"at the limit", `redirect "a@ext.test"; redirect "b@ext.test"; discard;`, "a@ext.test,b@ext.test", 0},
		{"one over", `redirect "a@ext.test"; redirect "b@ext.test"; redirect "c@ext.test"; discard;`, "", 1},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			relay := &replyRelay{}
			rs := goSmtp.NewServer(relay)
			rs.Domain = "relay.test"
			rl, err := new(net.ListenConfig).Listen(t.Context(), "tcp", "127.0.0.1:0")
			if err != nil {
				t.Fatal(err)
			}
			go rs.Serve(rl) //nolint:errcheck
			t.Cleanup(func() { rs.Close() })

			resolver := &mailbox.Resolver{Root: t.TempDir(), HomeTemplate: "%d/%n"}
			info := mustUserInfo(resolver, "alice@example.com")
			box := maildir.New().OpenUser(info)
			if err := box.Init(); err != nil {
				t.Fatal(err)
			}
			box.Close() //nolint:errcheck
			st := sieve.NewScriptStore("fs", "", nil, nil)
			ctx := context.Background()
			if err := st.SaveScript(ctx, "alice@example.com", info.Home, "fwd", []byte(c.script)); err != nil {
				t.Fatal(err)
			}
			if err := st.SetActive(ctx, "alice@example.com", info.Home, "fwd"); err != nil {
				t.Fatal(err)
			}
			scfg := config.Defaults().Sieve
			scfg.Enabled, scfg.MaxRedirects = true, 2
			scfg.SubmissionHost, scfg.SubmissionSSL, scfg.SubmissionTimeout = rl.Addr().String(), "no", 5

			srv := New(testOpts(Options{Hostname: "lmtp.test", Config: config.LMTPProtocolConfig{ReadTimeout: 5, WriteTimeout: 5},
				Mailbox: maildir.New(), Index: fileindex.New(), Resolver: resolver, SieveEngine: sieve.New(scfg, nil, nil, nil)}))
			ln, err := new(net.ListenConfig).Listen(ctx, "tcp", "127.0.0.1:0")
			if err != nil {
				t.Fatal(err)
			}
			t.Cleanup(func() { ln.Close() })
			go func() { _ = srv.Serve(ln) }()

			conn, sc := dialLMTP(t, ln.Addr().String())
			sendLHLO(t, conn, sc)
			transaction(t, conn, sc, "bob@ext.test", "")
			if got := relay.to(); got != c.wantSent {
				t.Errorf("sent to %q, want %q", got, c.wantSent)
			}
			if n := messagesIn(t, info, "INBOX"); n != c.wantInbox {
				t.Errorf("%d in INBOX, want %d", n, c.wantInbox)
			}
		})
	}
}
