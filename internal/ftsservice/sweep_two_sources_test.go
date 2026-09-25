//go:build flatcurve

package ftsservice

import (
	"os"
	"path/filepath"
	"testing"
)

// The GUID store is derived and rebuildable, so a row it is missing is not a
// message that is gone: a compaction must keep that message's document (#2030).
func TestOptimizeKeepsDocumentsTheStoreForgot(t *testing.T) {
	svc, box, uidx, root := newTestServiceIn(t)
	saveMessage(t, box, uidx, 1, "alpha")
	saveMessage(t, box, uidx, 2, "bravo")
	if err := svc.Index(testUser, testMbox, 2, 0); err != nil {
		t.Fatalf("index: %v", err)
	}
	waitIndexedIn(t, svc, testMbox, 2)

	// The records stay; the store loses what it knew about them, which is the
	// shape the stand was in: 25 live messages with no row (#2031).
	emptyGUIDStore(t, root)

	if err := svc.Optimize(testUser); err != nil {
		t.Fatalf("optimize: %v", err)
	}
	docs, _, messages, _, err := svc.Counts(testUser)
	if err != nil {
		t.Fatal(err)
	}
	if docs != messages {
		t.Errorf("the compaction left %d documents for %d live messages: a store gap read as deletion", docs, messages)
	}
}

// emptyGUIDStore takes the per-user store away, keeping the mail records.
func emptyGUIDStore(t *testing.T, root string) {
	t.Helper()
	var matches []string
	err := filepath.WalkDir(root, func(p string, d os.DirEntry, err error) error {
		if err == nil && !d.IsDir() && filepath.Base(p) == "yarilo.guid.index" {
			matches = append(matches, p)
		}
		return nil
	})
	if err != nil {
		t.Fatal(err)
	}
	if len(matches) == 0 {
		t.Fatalf("no guid store under %s", root)
	}
	for _, m := range matches {
		if err := os.Remove(m); err != nil {
			t.Fatalf("remove %s: %v", m, err)
		}
	}
}

// The fourth count is what makes a store gap visible: a live copy with no row
// cannot be resolved, so it is unsearchable however well it is indexed.
func TestCountsReportCopiesWithNoStoreRow(t *testing.T) {
	svc, box, uidx, root := newTestServiceIn(t)
	saveMessage(t, box, uidx, 1, "alpha")
	saveMessage(t, box, uidx, 2, "bravo")
	if err := svc.Index(testUser, testMbox, 2, 0); err != nil {
		t.Fatalf("index: %v", err)
	}
	waitIndexedIn(t, svc, testMbox, 2)

	if _, _, _, unrecorded, err := svc.Counts(testUser); err != nil {
		t.Fatal(err)
	} else if unrecorded != 0 {
		t.Errorf("a healthy account reports %d copies with no store row", unrecorded)
	}

	emptyGUIDStore(t, root)

	_, copies, _, unrecorded, err := svc.Counts(testUser)
	if err != nil {
		t.Fatal(err)
	}
	if unrecorded != copies {
		t.Errorf("with no store at all %d of %d copies are reported as recorded", copies-unrecorded, copies)
	}
}
