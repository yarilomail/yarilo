package jmap

import (
	"strings"
	"testing"

	"github.com/yarilomail/yarilo/internal/msgcache"
	"github.com/yarilomail/yarilo/internal/storage/index/file"
	"github.com/yarilomail/yarilo/pkg/mailbox"
)

// What JMAP writes back is what IMAP answers with: an Email/get must leave the
// envelope in the cache as the message wrote it, encoded word and all, or a
// later FETCH ENVELOPE hands the client decoded 8-bit text (#2008, #1714).
func TestEmailGetCachesTheEnvelopeAsTheMessageWroteIt(t *testing.T) {
	s, id, home := storedServerWithMessageAt(t, richMessage, 0)
	emailGet(t, s, `{"accountId":"u1@example.com","ids":["`+id+`"],
		"properties":`+envelopeProps+`}`)

	text := cachedEnvelopeText(t, home)
	if !strings.Contains(text, "=?utf-8?B?") {
		t.Errorf("the cached envelope carries %q, want the header's own encoded word", text)
	}
	if strings.Contains(text, "Привіт") {
		t.Errorf("the cached envelope was decoded: %q", text)
	}
}

// cachedEnvelopeText reads back what the cache holds for INBOX uid 1.
func cachedEnvelopeText(t *testing.T, home string) string {
	t.Helper()
	info := &mailbox.UserInfo{Username: testUser, Home: home, Separator: "/"}
	ui := file.New().OpenUser(info)
	defer ui.Close() //nolint:errcheck
	f, err := ui.OpenFolder("INBOX", 0)
	if err != nil {
		t.Fatalf("open folder: %v", err)
	}
	fc := msgcache.Open(ui, f.ID, msgcache.Options{})
	if fc == nil {
		t.Fatal("the folder has no envelope cache")
	}
	defer fc.Close() //nolint:errcheck
	msgs, err := ui.GetMessages(f.ID, mailbox.SeqSet{})
	if err != nil || len(msgs) == 0 {
		t.Fatalf("read records: %v", err)
	}
	text, ok := fc.EnvelopeText(msgs[0])
	if !ok {
		t.Fatal("Email/get left no envelope in the cache")
	}
	return text
}
