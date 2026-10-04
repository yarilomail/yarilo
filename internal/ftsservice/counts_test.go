//go:build flatcurve

package ftsservice

import (
	"strings"
	"testing"

	"github.com/yarilomail/yarilo/internal/storage/mailboxbase"
	"github.com/yarilomail/yarilo/pkg/fts"
	"github.com/yarilomail/yarilo/pkg/mailbox"
)

// The three numbers an operator reads must separate a copy from a message:
// one message in two folders is one document, two live copies, one message.
func TestCountsSeparateCopiesFromMessages(t *testing.T) {
	svc, box, uidx := newTestService(t)
	saveMessage(t, box, uidx, 1, "alpha")
	if err := svc.Index(testUser, testMbox, 1, 0); err != nil {
		t.Fatalf("index INBOX: %v", err)
	}
	waitIndexedIn(t, svc, testMbox, 1)
	guid := guidOfUID(t, uidx, 1)
	archive := copyInto(t, svc, box, uidx, "Archive", guid, 7)

	docs, copies, messages, _, err := svc.Counts(testUser)
	if err != nil {
		t.Fatalf("Counts: %v", err)
	}
	if docs != 1 {
		t.Errorf("documents = %d, want 1: a copy is not a second message", docs)
	}
	if copies != 2 {
		t.Errorf("live copies = %d, want 2 (INBOX and %s)", copies, archive.Name)
	}
	if messages != 1 {
		t.Errorf("live messages = %d, want 1 distinct GUID", messages)
	}
}

// copyInto files the same message into a folder and indexes it there. The
// folder may be one that already holds a copy: that is a copy too.
func copyInto(t *testing.T, svc *Service, box mailbox.UserMailbox, uidx mailbox.UserIndex, folder string, guid [16]byte, uid uint32) fts.MailboxRef {
	t.Helper()
	if exists, _ := box.FolderExists(folder); !exists {
		if err := box.Create(folder); err != nil {
			t.Fatal(err)
		}
	}
	raw := "From: a@test.com\r\nSubject: note alpha\r\n\r\nalpha\r\n"
	name, vsize, saved, err := box.Save(folder, strings.NewReader(raw), uid, int64(len(raw)), nil, nil, guid)
	if err != nil {
		t.Fatal(err)
	}
	if saved != guid {
		t.Fatalf("the copy was stored as %x, not as the message %x", saved, guid)
	}
	meta := &mailbox.MessageMeta{UID: uid, Size: uint32(len(raw)), VSize: vsize, GUID: guid}
	if err := mailboxbase.NameSaved(box, folder, name, meta); err != nil {
		t.Fatal(err)
	}
	f, err := uidx.OpenFolder(folder, testMbox.UIDValidity)
	if err != nil {
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
	ref := fts.MailboxRef{Name: folder, GUID: mailbox.FormatObjectID(f.GUID), UIDValidity: f.UIDValidity}
	if err := svc.Index(testUser, ref, uid, 0); err != nil {
		t.Fatalf("index %s: %v", folder, err)
	}
	// Indexing is a queued job: without the wait the copy is not in the index
	// yet, and a row about copies would be asking about one message.
	waitIndexedIn(t, svc, ref, uid)
	return ref
}
