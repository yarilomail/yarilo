package lmtp

import (
	"context"
	"fmt"
	"net"
	"path/filepath"
	"strings"
	"sync"
	"testing"

	fileindex "github.com/yarilomail/yarilo/v2/internal/storage/index/file"
	"github.com/yarilomail/yarilo/v2/internal/storage/mailbox/maildir"
	"github.com/yarilomail/yarilo/v2/pkg/config"
	"github.com/yarilomail/yarilo/v2/pkg/mailbox"
)

// A client that pipelines a batch and leaves without reading the replies gets
// at most the message in flight delivered, not the rest of the batch (#2615).
func TestASessionWhoseClientLeftStops(t *testing.T) {
	const batch = 20
	for _, tc := range []struct {
		name               string
		leave              bool
		wantAtMin, wantMax int
	}{
		{"a client that reads every reply gets the whole batch", false, batch, batch},
		{"a client that leaves gets at most the message in flight", true, 0, 1},
	} {
		t.Run(tc.name, func(t *testing.T) {
			ended := make(chan struct{})
			sessionEnded = func() { close(ended) }
			t.Cleanup(func() { sessionEnded = func() {} })

			dir := t.TempDir()
			gate := make(chan struct{})
			if !tc.leave {
				close(gate)
			}
			var first sync.Once
			srv := New(testOpts(Options{
				Hostname: "lmtp.test",
				Config:   config.LMTPProtocolConfig{ReadTimeout: 5, WriteTimeout: 5},
				Mailbox:  maildir.New(),
				Index:    fileindex.New(),
				Resolver: &mailbox.Resolver{Root: dir, HomeTemplate: "%d/%n"},
				UserdbLookup: func(_ context.Context, user string) (*mailbox.UserInfo, error) {
					first.Do(func() { <-gate })
					return &mailbox.UserInfo{Username: user, Home: filepath.Join(dir, user), Driver: "maildir"}, nil
				},
			}))
			ln, err := new(net.ListenConfig).Listen(t.Context(), "tcp", "127.0.0.1:0")
			if err != nil {
				t.Fatal(err)
			}
			t.Cleanup(func() { ln.Close() }) //nolint:errcheck
			go func() { _ = srv.Serve(ln) }()

			conn, sc := dialLMTP(t, ln.Addr().String())
			var b strings.Builder
			b.WriteString("LHLO mta.test\r\n")
			for i := 1; i <= batch; i++ {
				fmt.Fprintf(&b, "MAIL FROM:<a@external.test>\r\nRCPT TO:<alice@example.com>\r\nDATA\r\nSubject: %d\r\n\r\nbody\r\n.\r\n", i)
			}
			b.WriteString("QUIT\r\n")
			if _, err := conn.Write([]byte(b.String())); err != nil {
				t.Fatal(err)
			}
			if tc.leave {
				// Unread replies make the close a reset, as a sender that gave up does.
				conn.Close() //nolint:errcheck
				close(gate)
			} else {
				for sc.Scan() && !strings.HasPrefix(sc.Text(), "221") {
				}
				conn.Close() //nolint:errcheck
			}
			<-ended

			info := &mailbox.UserInfo{Username: "alice@example.com", Home: filepath.Join(dir, "alice@example.com"), Driver: "maildir"}
			got := 0
			idx := fileindex.New().OpenUser(info)
			defer idx.Close() //nolint:errcheck
			if f, err := idx.OpenFolder("INBOX", 0); err == nil {
				msgs, err := idx.GetMessages(f.ID, mailbox.SeqSet{{From: 1, To: 0}})
				if err != nil {
					t.Fatal(err)
				}
				got = len(msgs)
			}
			if got < tc.wantAtMin || got > tc.wantMax {
				t.Fatalf("delivered %d, want %d..%d", got, tc.wantAtMin, tc.wantMax)
			}
		})
	}
}
