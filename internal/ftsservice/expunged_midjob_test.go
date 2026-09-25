//go:build flatcurve

package ftsservice

import "testing"

// The race the counts found on the stand: a job reads the folder's records,
// the message is expunged, and on a store whose body outlives the record the
// fetch still succeeds -- so the document would be written for a message that
// is gone (#2026).
func TestAMessageExpungedMidJobIsNotWritten(t *testing.T) {
	svc, box, uidx := newTestService(t)
	saveMessage(t, box, uidx, 1, "alpha")
	saveMessage(t, box, uidx, 2, "bravo")

	// The job reads its record set here; the expunge lands before the write,
	// and the body stays readable, which is what mdbox does until a purge.
	svc.opts.beforeIndexWrite = func(uid uint32) {
		if uid != 1 {
			return
		}
		expungeCopy(t, uidx, testMbox.Name, 1)
	}

	if err := svc.Index(testUser, testMbox, 2, 0); err != nil {
		t.Fatalf("index: %v", err)
	}
	waitIndexedIn(t, svc, testMbox, 2)

	docs, _, messages, err := svc.Counts(testUser)
	if err != nil {
		t.Fatal(err)
	}
	if docs != messages {
		t.Errorf("the index holds %d documents for %d live messages: one was written for a message that went", docs, messages)
	}
	res, err := svc.Lookup(testUser, testMbox, lookupWord("alpha"))
	if err != nil {
		t.Fatal(err)
	}
	if len(res.Definite)+len(res.Maybe) != 0 {
		t.Errorf("the expunged message answers %v/%v", res.Definite, res.Maybe)
	}
}
