//go:build flatcurve

package ftsservice

import (
	"testing"

	"github.com/yarilomail/yarilo/pkg/fts"
	"github.com/yarilomail/yarilo/pkg/mailbox"
)

// Without a per-copy term the store is what says whether a retraction may take
// the folder or the document: these rows run through it, not around it.
func TestExpungeAsksTheStoreForWhatIsLeft(t *testing.T) {
	for _, tc := range []struct {
		name string
		// second is where the second copy of the same message is filed.
		second string
		// answers is the folder that must still answer after the expunge.
		answers string
	}{
		{"a copy in another folder", "Archive", "Archive"},
		{"a second copy in the same folder", "INBOX", "INBOX"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			svc, box, uidx := newTestService(t)
			saveMessage(t, box, uidx, 1, "alpha")
			if err := svc.Index(testUser, testMbox, 1, 0); err != nil {
				t.Fatalf("index INBOX: %v", err)
			}
			guid := guidOfUID(t, uidx, 1)
			second := copyInto(t, svc, box, uidx, tc.second, guid, 7)

			// The copy goes from the folder first, so the store has lost it
			// by the time the retraction asks what is left.
			expungeCopy(t, uidx, testMbox.Name, 1)
			if err := svc.Expunge(testUser, testMbox, 1, guid); err != nil {
				t.Fatalf("expunge: %v", err)
			}

			want := second
			if tc.answers == "INBOX" {
				want = testMbox
			}
			res, err := svc.Lookup(testUser, want, lookupWord("alpha"))
			if err != nil {
				t.Fatal(err)
			}
			if len(res.Definite)+len(res.Maybe) != 1 {
				t.Errorf("%s answers %v/%v after one copy went, want the copy it still holds",
					want.Name, res.Definite, res.Maybe)
			}
		})
	}
}

func expungeCopy(t *testing.T, uidx mailbox.UserIndex, folder string, uid uint32) {
	t.Helper()
	f, err := uidx.OpenFolder(folder, testMbox.UIDValidity)
	if err != nil {
		t.Fatal(err)
	}
	if err := uidx.ExpungeMessage(f.ID, uid); err != nil {
		t.Fatalf("expunge %s uid %d: %v", folder, uid, err)
	}
}

// A retraction may arrive before the store has caught up with it: the copy it
// names is then still recorded, and it must not read as one that stayed.
func TestExpungeIgnoresTheCopyItNames(t *testing.T) {
	svc, box, uidx := newTestService(t)
	saveMessage(t, box, uidx, 1, "alpha")
	if err := svc.Index(testUser, testMbox, 1, 0); err != nil {
		t.Fatalf("index INBOX: %v", err)
	}
	waitIndexedIn(t, svc, testMbox, 1)
	guid := guidOfUID(t, uidx, 1)

	// Deliberately without expunging the record first.
	if err := svc.Expunge(testUser, testMbox, 1, guid); err != nil {
		t.Fatalf("expunge: %v", err)
	}
	res, err := svc.Lookup(testUser, testMbox, lookupWord("alpha"))
	if err != nil {
		t.Fatal(err)
	}
	if len(res.Definite)+len(res.Maybe) != 0 {
		t.Errorf("INBOX answers %v/%v for the copy just retracted", res.Definite, res.Maybe)
	}
}

var _ = fts.MailboxRef{}
