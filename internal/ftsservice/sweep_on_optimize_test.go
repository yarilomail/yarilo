//go:build flatcurve

package ftsservice

import (
	"testing"

	"github.com/yarilomail/yarilo/pkg/mailbox"
)

// A retraction that never arrived must not outlive a compaction: the store is
// what says which messages are still there (#2026).
func TestOptimizeDropsDocumentsTheStoreNoLongerHas(t *testing.T) {
	svc, box, uidx := newTestService(t)
	saveMessage(t, box, uidx, 1, "alpha")
	saveMessage(t, box, uidx, 2, "bravo")
	if err := svc.Index(testUser, testMbox, 2, 0); err != nil {
		t.Fatalf("index: %v", err)
	}
	waitIndexedIn(t, svc, testMbox, 2)

	// The copy goes from the folder, and nothing tells the index: the lost
	// retraction this row is about.
	expungeCopy(t, uidx, testMbox.Name, 1)

	if err := svc.Optimize(testUser); err != nil {
		t.Fatalf("optimize: %v", err)
	}
	docs, _, messages, err := svc.Counts(testUser)
	if err != nil {
		t.Fatal(err)
	}
	if docs != messages {
		t.Errorf("after the compaction the index holds %d documents for %d live messages", docs, messages)
	}
	res, err := svc.Lookup(testUser, testMbox, lookupWord("alpha"))
	if err != nil {
		t.Fatal(err)
	}
	if len(res.Definite)+len(res.Maybe) != 0 {
		t.Errorf("the expunged message still answers: %v/%v", res.Definite, res.Maybe)
	}
}

// The same pass clears folder terms naming a mailbox the account no longer
// has, which no per-folder walk can reach (#2022). The folder holds a message
// of its own, so its documents are what the count is about.
func TestOptimizeSweepsOrphanFolders(t *testing.T) {
	svc, box, uidx := newTestService(t)
	saveMessage(t, box, uidx, 1, "alpha")
	if err := svc.Index(testUser, testMbox, 1, 0); err != nil {
		t.Fatalf("index: %v", err)
	}
	waitIndexedIn(t, svc, testMbox, 1)
	own := [16]byte{9, 9, 9, 9}
	copyInto(t, svc, box, uidx, "Archive", own, 7)

	// Archive goes from storage and from the mail index, as a DELETE that
	// never reached the search index leaves it.
	dropFolderRecords(t, box, uidx, "Archive")

	if err := svc.Optimize(testUser); err != nil {
		t.Fatalf("optimize: %v", err)
	}
	docs, _, messages, err := svc.Counts(testUser)
	if err != nil {
		t.Fatal(err)
	}
	if docs != messages {
		t.Errorf("the deleted folder left %d documents for %d live messages", docs, messages)
	}
	if got, err := svc.Lookup(testUser, testMbox, lookupWord("alpha")); err != nil {
		t.Fatal(err)
	} else if len(got.Definite)+len(got.Maybe) != 1 {
		t.Errorf("the sweep took INBOX's message too: %v/%v", got.Definite, got.Maybe)
	}
}

func dropFolderRecords(t *testing.T, box mailbox.UserMailbox, uidx mailbox.UserIndex, folder string) {
	t.Helper()
	f, err := uidx.OpenFolder(folder, testMbox.UIDValidity)
	if err != nil {
		t.Fatal(err)
	}
	_ = f
	if err := uidx.DeleteFolder(folder); err != nil {
		t.Fatalf("delete folder index: %v", err)
	}
	if err := box.Delete(folder); err != nil {
		t.Fatalf("delete folder: %v", err)
	}
}
