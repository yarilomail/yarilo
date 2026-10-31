//go:build flatcurve

package main

import (
	"testing"
	"time"

	"github.com/yarilomail/yarilo/v2/internal/fts/flatcurve"
	"github.com/yarilomail/yarilo/v2/internal/fts/language"
	"github.com/yarilomail/yarilo/v2/internal/ftsservice"
	imapserver "github.com/yarilomail/yarilo/v2/internal/imap"
	"github.com/yarilomail/yarilo/v2/internal/jmap"
	"github.com/yarilomail/yarilo/v2/internal/lmtp"
	"github.com/yarilomail/yarilo/v2/internal/storage/index/file"
	"github.com/yarilomail/yarilo/v2/internal/storage/mailbox/maildir"
	"github.com/yarilomail/yarilo/v2/pkg/ftsproto"
)

// withFTS is the stand with yarilo-fts in process, the flatcurve engine behind
// it, and every surface wired to it as a deployment wires them.
func (s *localStand) withFTS(t *testing.T) {
	t.Helper()
	set := language.DefaultSettings()
	chain, err := language.NewMultiChain([]string{set.Language}, set.Filters, nil, set.TokenMaxLen, set.AddressMaxLen, 0)
	if err != nil {
		t.Fatal(err)
	}
	svc, err := ftsservice.New(ftsservice.Options{
		Engine:      flatcurve.New(flatcurve.Options{CommitLimit: 500, MinTermSize: 2, RotateCount: 5000}),
		Mailbox:     maildir.New(),
		Index:       file.New(file.WithLocker(s.locker)),
		ResolveUser: s.userInfo,
		Chain:       chain,
		CommitLimit: 2,
		Workers:     1,
	})
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = svc.Close() })
	client := ftsproto.Embedded{Service: svc}
	s.withIMAP(t, func(o *imapserver.Options) {
		o.FTS = imapserver.FTSOptions{Client: client, Chain: chain, AddMissing: "body-search-only",
			ReadFallback: true, Timeout: 10 * time.Second, Autoindex: true, SearchEnabled: true}
	})
	s.withLMTP(t, func(o *lmtp.Options) { o.FTSClient, o.FTSAutoindex = client, true })
	s.withJMAP(t, func(o *jmap.Options) {
		o.FTS = &jmap.FTS{Client: client, Chain: chain, MaxConns: 4, MaxFolders: 100, AddMissing: "body-search-only", Timeout: 10 * time.Second}
	})
	s.withBackendAPI(t, client)
	setFlag(t, flagFTSUser, standUser)
	setFlag(t, flagFTSPass, standPass)
}

// The rows that find a message by its words, against the servers and yarilo-fts.
func TestFTSAndConsistencyRowsAgainstTheServers(t *testing.T) {
	s := startStand(t)
	s.withFTS(t)
	rows := []struct {
		name string
		fn   func() error
	}{
		{"imap fts search", func() error { return checkFTS(standUser, standPass) }},
		{"fts document is the message", func() error { return checkFTSDocumentIsMessage(standUser, standPass, true) }},
		{"jmap query over fts", func() error { return checkJMAPFTSQuery(standUser) }},
		{"one lmtp delivery seen by every reader", func() error { return checkConsistencyReaders(standUser, standPass) }},
		{"imap<->jmap identity", func() error { return checkConsistencyIdentity(standUser, standPass) }},
		{"imap<->jmap message-id", func() error { return checkConsistencyMessageID(standUser, standPass) }},
		{"imap flag -> jmap keyword", func() error { return checkConsistencyFlagsIMAPToJMAP(standUser, standPass) }},
		{"jmap keyword -> imap flag", func() error { return checkConsistencyFlagsJMAPToIMAP(standUser, standPass) }},
		{"jmap changes", func() error { return checkConsistencyChanges(standUser, standPass) }},
		{"imap flag -> eventsource", func() error { return checkConsistencyPush(standUser, standPass) }},
		{"imap flag -> websocket", func() error { return checkConsistencyWebSocket(standUser, standPass) }},
		{"imap search <-> jmap query", func() error { return checkConsistencySearch(standUser, standPass) }},
	}
	for _, r := range rows {
		t.Run(r.name, func(t *testing.T) {
			if err := r.fn(); err != nil {
				t.Fatal(err)
			}
		})
	}
}
