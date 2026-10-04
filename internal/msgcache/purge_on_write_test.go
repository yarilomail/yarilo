package msgcache

import (
	"testing"

	imaplib "github.com/emersion/go-imap/v2"

	"github.com/yarilomail/yarilo/internal/storage/index/file"
	"github.com/yarilomail/yarilo/internal/storage/mailindex"
	"github.com/yarilomail/yarilo/pkg/mailbox"
)

// A window that appends asks for a purge before its locks go, as the reference
// asks on every header write: continued records alone, with no expunge, purge.
func TestAWindowThatWritesPurgesPastTheContinuedShare(t *testing.T) {
	for _, tc := range []struct {
		name      string
		pct       int
		wantPurge bool
	}{
		{"at: 8 continued over 4 messages is 200%", 200, true},
		{"one short of it", 201, false},
	} {
		t.Run(tc.name, func(t *testing.T) {
			idx := file.New(file.WithCachePurgeDeletePercentage(-1), file.WithCachePurgeContinuedPercentage(tc.pct),
				file.WithCachePurgeMinSize(0)).OpenUser(&mailbox.UserInfo{Username: "u", Home: t.TempDir()})
			f, err := idx.OpenFolder("INBOX", 7)
			if err != nil {
				t.Fatal(err)
			}
			for uid := uint32(1); uid <= 4; uid++ {
				if err := idx.AppendMessage(f.ID, &mailbox.MessageMeta{UID: uid}); err != nil {
					t.Fatal(err)
				}
			}
			store := func(write func(*Handle, *mailbox.MessageMeta)) {
				msgs, err := idx.GetMessages(f.ID, mailbox.SeqSet{})
				if err != nil {
					t.Fatal(err)
				}
				h := Open(idx, f.ID, Options{})
				if h == nil {
					t.Fatal("no cache handle")
				}
				for _, m := range msgs {
					write(h, m)
				}
				h.Close()
			}
			store(func(h *Handle, m *mailbox.MessageMeta) { h.StoreEnvelope(m, &imaplib.Envelope{Subject: "s"}) })
			before := header(t, idx, f.ID)
			store(func(h *Handle, m *mailbox.MessageMeta) {
				h.StoreBodyStructure(m, &imaplib.BodyStructureSinglePart{Type: "text", Subtype: "plain"})
			})
			if before.ContinuedRecordCount != 0 {
				t.Fatalf("the first window left %d continued records, want none", before.ContinuedRecordCount)
			}
			after := header(t, idx, f.ID)
			if purged := after.FileSeq != before.FileSeq; purged != tc.wantPurge {
				t.Errorf("8 continued over 4 messages under %d%%: purged %v, want %v", tc.pct, purged, tc.wantPurge)
			}
		})
	}
}

func header(t *testing.T, idx mailbox.UserIndex, fid uint64) mailindex.CacheHeader {
	t.Helper()
	ic := idx.(Index)
	indexID, resetID, _, err := ic.CachePairIdentity(fid)
	if err != nil {
		t.Fatal(err)
	}
	path, _ := ic.CachePath(fid)
	cf, err := mailindex.OpenCache(path, indexID, resetID)
	if err != nil {
		t.Fatal(err)
	}
	defer cf.Close() //nolint:errcheck
	return cf.Header()
}
