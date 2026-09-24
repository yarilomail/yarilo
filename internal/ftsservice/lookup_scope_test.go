//go:build flatcurve

package ftsservice

import (
	"encoding/hex"
	"strings"
	"testing"
	"time"

	"github.com/yarilomail/yarilo/internal/storage/mailboxbase"
	"github.com/yarilomail/yarilo/pkg/fts"
	"github.com/yarilomail/yarilo/pkg/mailbox"
)

// Row 5, at the service's seam: a search names the folder it searches, and a
// hit is the uid of a copy in that folder (#1986).
func TestAServiceLookupAnswersOnlyTheFolderAsked(t *testing.T) {
	svc, box, uidx := newTestService(t)
	saveMessage(t, box, uidx, 1, "wolves howl nightly")
	other := saveMessageIn(t, box, uidx, "Archive", 1, "wolves rest")

	if err := svc.Index(testUser, testMbox, 1, 0); err != nil {
		t.Fatal(err)
	}
	waitIndexed(t, svc, 1)
	if err := svc.Index(testUser, other, 1, 0); err != nil {
		t.Fatal(err)
	}
	// The second folder's job is queued the same way: wait for its checkpoint
	// rather than for the first folder's.
	waitIndexedIn(t, svc, other, 1)

	res, err := svc.Lookup(testUser, testMbox, lookupWord("wolv"))
	if err != nil {
		t.Fatal(err)
	}
	if len(res.Definite) != 1 || res.Definite[0] != 1 {
		t.Fatalf("INBOX answers %v, want its own uid 1", res.Definite)
	}

	res, err = svc.Lookup(testUser, other, lookupWord("wolv"))
	if err != nil {
		t.Fatal(err)
	}
	if len(res.Definite) != 1 || res.Definite[0] != 1 {
		t.Fatalf("Archive answers %v, want its own uid 1", res.Definite)
	}

	// A word only the other folder holds is not an answer here.
	res, err = svc.Lookup(testUser, testMbox, lookupWord("rest"))
	if err != nil {
		t.Fatal(err)
	}
	if len(res.Definite) != 0 {
		t.Errorf("INBOX answered %v for a word only Archive holds", res.Definite)
	}
}

// saveMessageIn stores a message in another folder and answers with that
// folder's ref, so a row can ask each folder its own question.
func saveMessageIn(t *testing.T, box mailbox.UserMailbox, uidx mailbox.UserIndex, name string, uid uint32, body string) fts.MailboxRef {
	t.Helper()
	if err := box.Create(name); err != nil {
		t.Fatal(err)
	}
	f, err := uidx.OpenFolder(name, 0)
	if err != nil {
		t.Fatal(err)
	}
	raw := "From: a@test.com\r\nSubject: note " + body + "\r\n\r\n" + body + "\r\n"
	saved, vsize, guid, err := box.Save(name, strings.NewReader(raw), uid, int64(len(raw)), nil, nil, [16]byte{})
	if err != nil {
		t.Fatal(err)
	}
	meta := &mailbox.MessageMeta{UID: uid, Size: uint32(len(raw)), VSize: vsize, GUID: guid}
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
	// The folder's own uidvalidity: the service refuses a checkpoint recorded
	// under another one, and a row that invents it waits forever.
	return fts.MailboxRef{Name: name, GUID: mailbox.FormatObjectID(f.GUID), UIDValidity: f.UIDValidity}
}

// waitIndexedIn waits for one folder's checkpoint to reach uid.
func waitIndexedIn(t *testing.T, svc *Service, mbox fts.MailboxRef, uid uint32) {
	t.Helper()
	deadline := time.Now().Add(10 * time.Second)
	for time.Now().Before(deadline) {
		if last, _, err := svc.Status(testUser, mbox); err == nil && last >= uid {
			return
		}
		time.Sleep(20 * time.Millisecond)
	}
	t.Fatalf("folder %q never reached uid %d", mbox.Name, uid)
}

// scopeIndex records the folders a lookup was scoped to.
type scopeIndex struct {
	stubUserIndex
	asked [][]string
}

func (s *scopeIndex) Lookup(folders []string, _ fts.Query) (fts.Result, error) {
	s.asked = append(s.asked, folders)
	return fts.Result{}, nil
}

// The service asks the engine for the folder it was given. Searching the whole
// account and filtering afterwards answers the same, and reads every folder's
// postings to do it (#1986).
func TestTheServiceScopesTheLookupToTheFolder(t *testing.T) {
	idx := &scopeIndex{}
	h := &userHandle{ui: idx}
	s := &Service{opts: Options{}}
	mbox := fts.MailboxRef{Name: "INBOX", GUID: "0f0e0d0c0b0a09080706050403020100"}

	if _, err := s.lookupThrough(h, mbox, fts.Query{}); err != nil {
		t.Fatal(err)
	}
	if len(idx.asked) != 1 || len(idx.asked[0]) != 1 || idx.asked[0][0] != mbox.GUID {
		t.Errorf("the engine was asked for %v, want the folder that was searched", idx.asked)
	}
}

// Row 6: the uid of a hit comes from the GUID store, not from the document.
// The engine answers with messages; what the folder calls them is the store's.
func TestAHitIsResolvedThroughTheStore(t *testing.T) {
	svc, box, uidx := newTestService(t)
	saveMessage(t, box, uidx, 1, "wolves howl nightly")
	if err := svc.Index(testUser, testMbox, 1, 0); err != nil {
		t.Fatal(err)
	}
	waitIndexed(t, svc, 1)

	res, err := svc.Lookup(testUser, testMbox, lookupWord("wolv"))
	if err != nil {
		t.Fatal(err)
	}
	if len(res.Definite) != 1 {
		t.Fatalf("the search answers %v, want one uid", res.Definite)
	}

	// Rewrite the store so the copy is recorded under another uid: the answer
	// follows the store, which is what makes it the store's to say.
	rebuilder, ok := uidx.(mailbox.GUIDStoreRebuilder)
	if !ok {
		t.Fatal("this index rebuilds no GUID store")
	}
	resolver := uidx.(mailbox.GUIDResolver)
	copies, err := resolver.GUIDCopies(nil)
	if err != nil {
		t.Fatal(err)
	}
	_ = copies
	f, err := uidx.OpenFolder(testMbox.Name, testMbox.UIDValidity)
	if err != nil {
		t.Fatal(err)
	}
	metas, err := uidx.GetMessages(f.ID, mailbox.SeqSet{{From: 1, To: 0}})
	if err != nil || len(metas) != 1 {
		t.Fatalf("the folder holds %v (err %v)", metas, err)
	}
	folderGUID, err := hex.DecodeString(testMbox.GUID)
	if err != nil {
		t.Fatal(err)
	}
	var fg [16]byte
	copy(fg[:], folderGUID)
	if err := rebuilder.ReplaceGUIDStore([]mailbox.GUIDRecord{{
		GUID: metas[0].GUID, FolderGUID: fg, UID: 42,
	}}); err != nil {
		t.Fatal(err)
	}

	svc.Close() //nolint:errcheck
	res, err = svc.Lookup(testUser, testMbox, lookupWord("wolv"))
	if err != nil {
		t.Fatal(err)
	}
	if len(res.Definite) != 1 || res.Definite[0] != 42 {
		t.Errorf("the search answers %v, want the uid the store records", res.Definite)
	}
}
