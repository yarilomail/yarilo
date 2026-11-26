package maildir

import (
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"testing"

	"github.com/prometheus/client_golang/prometheus/testutil"

	"github.com/yarilomail/yarilo/v2/pkg/mailbox"
)

// twoSessions opens the same user twice, as two sessions of one process do:
// two userMailboxes, each with its own mutex, over one folder.
func twoSessions(t *testing.T) (*userMailbox, *userMailbox) {
	t.Helper()
	a := batchBox(t)
	info := &mailbox.UserInfo{Username: a.username, Home: a.home}
	b := New().OpenUser(info).(*userMailbox)
	if err := b.Init(); err != nil {
		t.Fatal(err)
	}
	return a, b
}

// together runs fns at once, as near the same instant as goroutines allow.
func together(fns ...func()) {
	var start, done sync.WaitGroup
	start.Add(1)
	for _, fn := range fns {
		done.Add(1)
		go func() {
			defer done.Done()
			start.Wait()
			fn()
		}()
	}
	start.Done()
	done.Wait()
}

// lettersOf returns the keyword letters in a maildir name's info part.
func lettersOf(name string) string {
	_, info, ok := strings.Cut(filepath.Base(name), ":2,")
	if !ok {
		return ""
	}
	var out []byte
	for i := 0; i < len(info); i++ {
		if info[i] >= 'a' && info[i] <= 'z' {
			out = append(out, info[i])
		}
	}
	return string(out)
}

func letterOf(t *testing.T, box *userMailbox, folder, keyword string) byte {
	t.Helper()
	var found []byte
	for letter, name := range box.keywordNames(folder) {
		if name == keyword {
			found = append(found, letter)
		}
	}
	if len(found) != 1 {
		t.Fatalf("%s: the keyword file names %q under %d letters %q, want 1", folder, keyword, len(found), found)
	}
	return found[0]
}

const rounds = 40

// Two sessions saving with the same new keyword at once: no error, and the
// keyword has one letter, which both names carry (#2733).
func TestTwoSessionsAddingOneNewKeywordGetOneLetter(t *testing.T) {
	a, b := twoSessions(t)
	for i := range rounds {
		folder := fmt.Sprintf("F%d", i)
		if err := a.Create(folder); err != nil {
			t.Fatal(err)
		}
		kw := fmt.Sprintf("$k%d", i)
		names := make([]string, 2)
		errs := make([]error, 2)
		together(
			func() {
				names[0], _, _, errs[0] = a.Save(folder, strings.NewReader("From: a@b\r\n\r\nx\r\n"), 0, 0, nil, []string{kw}, [16]byte{})
			},
			func() {
				names[1], _, _, errs[1] = b.Save(folder, strings.NewReader("From: a@b\r\n\r\ny\r\n"), 0, 0, nil, []string{kw}, [16]byte{})
			},
		)
		for s, err := range errs {
			if err != nil {
				t.Fatalf("round %d, session %d: %v", i, s, err)
			}
		}
		letter := letterOf(t, a, folder, kw)
		for s, name := range names {
			if got := lettersOf(name); got != string(letter) {
				t.Errorf("round %d, session %d: name %q carries %q, want %q", i, s, name, got, string(letter))
			}
		}
	}
}

// Two sessions saving with different new keywords at once: two letters, both
// keywords in the file, and each message names its own keyword's letter. With
// the file read and written by each session alone, both took the same free
// letter and the later rename won (#2733).
func TestTwoSessionsAddingDifferentKeywordsKeepBoth(t *testing.T) {
	a, b := twoSessions(t)
	for i := range rounds {
		folder := fmt.Sprintf("F%d", i)
		if err := a.Create(folder); err != nil {
			t.Fatal(err)
		}
		kws := []string{fmt.Sprintf("$a%d", i), fmt.Sprintf("$b%d", i)}
		names := make([]string, 2)
		errs := make([]error, 2)
		together(
			func() {
				names[0], _, _, errs[0] = a.Save(folder, strings.NewReader("From: a@b\r\n\r\nx\r\n"), 0, 0, nil, kws[:1], [16]byte{})
			},
			func() {
				names[1], _, _, errs[1] = b.Save(folder, strings.NewReader("From: a@b\r\n\r\ny\r\n"), 0, 0, nil, kws[1:], [16]byte{})
			},
		)
		for s, err := range errs {
			if err != nil {
				t.Fatalf("round %d, session %d: %v", i, s, err)
			}
		}
		la, lb := letterOf(t, a, folder, kws[0]), letterOf(t, a, folder, kws[1])
		if la == lb {
			t.Fatalf("round %d: %q and %q share the letter %q", i, kws[0], kws[1], string(la))
		}
		for s, want := range []byte{la, lb} {
			if got := lettersOf(names[s]); got != string(want) {
				t.Errorf("round %d, session %d: name %q carries %q, want %q (%s)", i, s, names[s], got, string(want), kws[s])
			}
		}
	}
}

// A keyword that already has a letter takes no lock: the usual case costs
// what it did before (#2733).
func TestAKnownKeywordTakesNoLock(t *testing.T) {
	box := batchBox(t)
	if _, _, _, err := box.Save("INBOX", strings.NewReader("From: a@b\r\n\r\nx\r\n"), 0, 0, nil, []string{"$Known"}, [16]byte{}); err != nil {
		t.Fatal(err)
	}
	before := testutil.ToFloat64(metricLockAcquired.WithLabelValues(lockSiteKeywords))
	name, _, _, err := box.Save("INBOX", strings.NewReader("From: a@b\r\n\r\ny\r\n"), 0, 0, nil, []string{"$Known"}, [16]byte{})
	if err != nil {
		t.Fatal(err)
	}
	if got := testutil.ToFloat64(metricLockAcquired.WithLabelValues(lockSiteKeywords)) - before; got != 0 {
		t.Errorf("a save with a known keyword took the list %v times, want 0", got)
	}
	if lettersOf(name) == "" {
		t.Errorf("name %q carries no keyword letter", name)
	}
	before = testutil.ToFloat64(metricLockAcquired.WithLabelValues(lockSiteKeywords))
	if _, _, _, err := box.Save("INBOX", strings.NewReader("From: a@b\r\n\r\nz\r\n"), 0, 0, nil, []string{"$New"}, [16]byte{}); err != nil {
		t.Fatal(err)
	}
	if got := testutil.ToFloat64(metricLockAcquired.WithLabelValues(lockSiteKeywords)) - before; got != 1 {
		t.Errorf("a save with a new keyword took the list %v times, want 1", got)
	}
}

// STORE of new keywords from two sessions at once, one message each: every
// keyword reaches its message's name, not the index alone (#2733: the
// lk2712 run logged "could not record flags in storage" on this path).
func TestTwoSessionsStoringNewKeywordsNameTheirMessages(t *testing.T) {
	a, b := twoSessions(t)
	for i := range rounds {
		folder := fmt.Sprintf("F%d", i)
		if err := a.Create(folder); err != nil {
			t.Fatal(err)
		}
		files := []string{
			fmt.Sprintf("17000%03d1.M1Pa.host:2,", i),
			fmt.Sprintf("17000%03d2.M1Pb.host:2,", i),
		}
		for _, f := range files {
			p := filepath.Join(a.folderPath(folder), "cur", f)
			if err := os.WriteFile(p, []byte("From: a@b\r\n\r\nx\r\n"), 0o600); err != nil {
				t.Fatal(err)
			}
		}
		kws := []string{fmt.Sprintf("$a%d", i), fmt.Sprintf("$b%d", i)}
		names := make([]string, 2)
		errs := make([]error, 2)
		together(
			func() { names[0], errs[0] = a.WriteFlags(folder, files[0], []string{`\Seen`}, kws[:1]) },
			func() {
				r := b.WriteFlagsMulti(folder, []mailbox.FlagWrite{{UID: 2, Filename: files[1], Flags: []string{`\Seen`}, Keywords: kws[1:]}})
				names[1], errs[1] = r[0].Filename, r[0].Err
			},
		)
		for s, err := range errs {
			if err != nil {
				t.Fatalf("round %d, session %d: %v", i, s, err)
			}
		}
		la, lb := letterOf(t, a, folder, kws[0]), letterOf(t, a, folder, kws[1])
		for s, want := range []byte{la, lb} {
			if got := lettersOf(names[s]); got != string(want) {
				t.Errorf("round %d, session %d: name %q carries %q, want %q (%s)", i, s, names[s], got, string(want), kws[s])
			}
		}
	}
}
