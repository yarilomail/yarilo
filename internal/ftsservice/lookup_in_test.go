//go:build flatcurve

package ftsservice

import (
	"sort"
	"strings"
	"testing"

	"github.com/yarilomail/yarilo/internal/storage/mailboxbase"
	"github.com/yarilomail/yarilo/pkg/fts"
	"github.com/yarilomail/yarilo/pkg/mailbox"
)

// saveCopyIn files a message already stored elsewhere: the same GUID in a
// second folder is what a copy is.
func saveCopyIn(t *testing.T, box mailbox.UserMailbox, uidx mailbox.UserIndex, name string, uid uint32, body string, guid [16]byte) fts.MailboxRef {
	t.Helper()
	box.Create(name) //nolint:errcheck
	f, err := uidx.OpenFolder(name, 0)
	if err != nil {
		t.Fatal(err)
	}
	raw := "From: a@test.com\r\nSubject: note " + body + "\r\n\r\n" + body + "\r\n"
	saved, vsize, got, err := box.Save(name, strings.NewReader(raw), uid, int64(len(raw)), nil, nil, guid)
	if err != nil {
		t.Fatal(err)
	}
	meta := &mailbox.MessageMeta{UID: uid, Size: uint32(len(raw)), VSize: vsize, GUID: got}
	if err := mailboxbase.NameSaved(box, name, saved, meta); err != nil {
		t.Fatal(err)
	}
	tx, err := uidx.Begin(f.ID)
	if err != nil {
		t.Fatal(err)
	}
	tx.Append(meta)
	if _, err := tx.Commit(); err != nil {
		t.Fatal(err)
	}
	return fts.MailboxRef{Name: name, GUID: mailbox.FormatObjectID(f.GUID), UIDValidity: f.UIDValidity}
}

func pairs(hits []fts.FolderHit, names map[string]string) []string {
	var out []string
	for _, h := range hits {
		out = append(out, names[h.Folder]+":"+string(rune('0'+h.UID)))
	}
	sort.Strings(out)
	return out
}

// One search over a set answers each copy inside it: a message in two folders
// of the set is two hits, and a folder outside the set answers nothing.
func TestLookupInAnswersEveryCopyInTheSet(t *testing.T) {
	svc, box, uidx := newTestService(t)
	saveMessage(t, box, uidx, 1, "wolves howl nightly")
	f, err := uidx.OpenFolder(testMbox.Name, testMbox.UIDValidity)
	if err != nil {
		t.Fatal(err)
	}
	metas, err := uidx.GetMessages(f.ID, mailbox.SeqSet{{From: 1, To: 0}})
	if err != nil || len(metas) != 1 {
		t.Fatalf("INBOX holds %v (err %v)", metas, err)
	}
	archive := saveCopyIn(t, box, uidx, "Archive", 3, "wolves howl nightly", metas[0].GUID)
	outside := saveCopyIn(t, box, uidx, "Outside", 1, "wolves outside", [16]byte{})

	for _, m := range []fts.MailboxRef{testMbox, archive, outside} {
		last := uint32(1)
		if m.Name == "Archive" {
			last = 3
		}
		if err := svc.Index(testUser, m, last, 0); err != nil {
			t.Fatal(err)
		}
		waitIndexedIn(t, svc, m, last)
	}
	names := map[string]string{testMbox.GUID: "INBOX", archive.GUID: "Archive", outside.GUID: "Outside"}

	for _, tc := range []struct {
		name string
		set  []fts.MailboxRef
		want string
	}{
		{"both copies in the set", []fts.MailboxRef{testMbox, archive}, "Archive:3,INBOX:1"},
		{"one copy in the set", []fts.MailboxRef{archive}, "Archive:3"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			res, err := svc.LookupIn(testUser, tc.set, lookupWord("wolv"))
			if err != nil {
				t.Fatal(err)
			}
			if got := strings.Join(pairs(res.Definite, names), ","); got != tc.want {
				t.Errorf("the set answers %s, want %s", got, tc.want)
			}
		})
	}
}
