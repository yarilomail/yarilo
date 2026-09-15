package maildir

import (
	"path/filepath"
	"strings"
	"testing"

	"github.com/prometheus/client_golang/prometheus/testutil"

	"github.com/yarilomail/yarilo/pkg/mailbox"
)

func batchBox(t *testing.T) *userMailbox {
	t.Helper()
	root := t.TempDir()
	const user = "u@x.com"
	info := &mailbox.UserInfo{Username: user, Home: testHome(root, user)}
	box := New().OpenUser(info).(*userMailbox)
	if err := box.Init(); err != nil {
		t.Fatal(err)
	}
	if err := box.Create("INBOX"); err != nil {
		t.Fatal(err)
	}
	return box
}

// A STORE takes no lock at all: it renames the files it names, and a rename
// excludes nobody -- two sessions storing different messages never wait (#1840).
func TestAStoreTakesNoLock(t *testing.T) {
	box := batchBox(t)
	const n = 20
	writes := make([]mailbox.FlagWrite, 0, n)
	for i := 0; i < n; i++ {
		name := "170000000" + string(rune('0'+i%10)) + ".M1P" + string(rune('a'+i)) + ".host:2,"
		deliverToCur(t, box, name, "From: a@b\r\n\r\nx\r\n")
		writes = append(writes, mailbox.FlagWrite{
			UID: uint32(i + 1), Filename: name, Flags: []string{`\Seen`}, Keywords: []string{"$Important"},
		})
	}
	before := holdsTaken(t)
	results := box.WriteFlagsMulti("INBOX", writes)
	if got := holdsTaken(t) - before; got != 0 {
		t.Errorf("the batch took a file lock %d times, want 0", got)
	}
	if len(results) != n {
		t.Fatalf("got %d results for %d writes", len(results), n)
	}
	for _, r := range results {
		if r.Err != nil {
			t.Errorf("uid %d: %v", r.UID, r.Err)
		}
		if !strings.Contains(r.Filename, "S") {
			t.Errorf("uid %d: name %q carries no \\Seen", r.UID, r.Filename)
		}
	}
}

// holdsTaken sums the file locks this package has taken, across every site.
func holdsTaken(t *testing.T) int {
	t.Helper()
	total := 0.0
	for _, site := range []string{lockSiteSave, lockSiteReconcileApply} {
		total += testutil.ToFloat64(metricLockAcquired.WithLabelValues(site))
	}
	return int(total)
}

// The keyword file is read once for the batch, not once per message.
//
// The lock was the expensive half and is fixed by the batch; this is the other
// half, and it is the one a comment can claim without the code doing it. Counted
// by watching the file itself: every open of it is a read (#1623).
func TestABatchReadsTheKeywordFileOnce(t *testing.T) {
	box := batchBox(t)
	const n = 12
	writes := make([]mailbox.FlagWrite, 0, n)
	for i := 0; i < n; i++ {
		name := "1700000" + string(rune('0'+i%10)) + "0.M1P" + string(rune('a'+i)) + ".host:2,"
		deliverToCur(t, box, name, "From: a@b\r\n\r\nx\r\n")
		// A keyword of its own per message, so every one of them would have to
		// allocate a letter and rewrite the file if this were done per message.
		writes = append(writes, mailbox.FlagWrite{
			UID: uint32(i + 1), Filename: name, Keywords: []string{"$kw" + string(rune('a'+i))},
		})
	}
	path := filepath.Join(box.folderPath("INBOX"), keywordsFileName)

	reads := 0
	restore := onKeywordFileRead(func(p string) {
		if p == path {
			reads++
		}
	})
	defer restore()

	for _, r := range box.WriteFlagsMulti("INBOX", writes) {
		if r.Err != nil {
			t.Fatalf("uid %d: %v", r.UID, r.Err)
		}
	}
	if reads != 1 {
		t.Errorf("the batch read the keyword file %d times, want 1", reads)
	}

	// And every keyword still got its own letter, in one file.
	names := box.keywordNames("INBOX")
	if len(names) != n {
		t.Errorf("the keyword file names %d keywords, want %d", len(names), n)
	}
}

// A message whose file is gone does not take the rest of the batch with it.
func TestABatchWithOneMissingFileWritesTheRest(t *testing.T) {
	box := batchBox(t)
	names := []string{
		"1700000001.M1Pa.host:2,",
		"1700000002.M1Pb.host:2,",
		"1700000003.M1Pc.host:2,",
	}
	for _, n := range names {
		deliverToCur(t, box, n, "From: a@b\r\n\r\nx\r\n")
	}
	writes := []mailbox.FlagWrite{
		{UID: 1, Filename: names[0], Flags: []string{`\Seen`}},
		{UID: 2, Filename: "1700000099.MgoneP.host:2,", Flags: []string{`\Seen`}},
		{UID: 3, Filename: names[2], Flags: []string{`\Seen`}},
	}
	results := box.WriteFlagsMulti("INBOX", writes)
	for i, r := range results {
		if r.Err != nil {
			t.Errorf("result %d carries an error: %v", i, r.Err)
		}
	}
	// The two that exist were renamed; the missing one keeps the name the
	// index has, for the reconcile to settle.
	for _, i := range []int{0, 2} {
		if !strings.Contains(results[i].Filename, "S") {
			t.Errorf("uid %d: name %q was not written", results[i].UID, results[i].Filename)
		}
	}
	if results[1].Filename != writes[1].Filename {
		t.Errorf("the missing message came back as %q", results[1].Filename)
	}
}

// onKeywordFileRead installs a counter for reads of a folder's keyword file and
// returns the call that removes it.
func onKeywordFileRead(fn func(path string)) func() {
	keywordFileRead = fn
	return func() { keywordFileRead = nil }
}
