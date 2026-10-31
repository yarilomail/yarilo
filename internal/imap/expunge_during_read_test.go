package imap_test

import (
	"errors"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"

	imaplib "github.com/emersion/go-imap/v2"
	"github.com/emersion/go-imap/v2/imapclient"

	"github.com/yarilomail/yarilo/v2/internal/storage/mailbox/maildir"
)

// expungeOnOpen has b expunge message n, then runs after, when a read reaches
// the gap between a record's lookup and its open for the at-th time.
func expungeOnOpen(t *testing.T, b *imapclient.Client, n uint32, at int, after func()) func() {
	t.Helper()
	var mu sync.Mutex
	opens := 0
	return maildir.SetTestBeforeOpen(func() {
		mu.Lock()
		opens++
		mine := opens == at
		mu.Unlock()
		if !mine {
			return
		}
		done := make(chan error, 1)
		go func() {
			if err := b.Store(imaplib.SeqSetNum(n), &imaplib.StoreFlags{Op: imaplib.StoreFlagsAdd, Flags: []imaplib.Flag{imaplib.FlagDeleted}}, nil).Close(); err != nil {
				done <- err
				return
			}
			done <- b.Expunge().Close()
		}()
		select {
		case err := <-done:
			if err != nil {
				t.Error(err)
			}
			if after != nil {
				after()
			}
		case <-time.After(5 * time.Second):
			t.Error("the other session's expunge did not finish")
		}
	})
}

func twoSessionsOnTwoMessages(t *testing.T) (a, b *imapclient.Client, dir string) {
	t.Helper()
	dir = t.TempDir()
	a, b = startTestServerIn(t, dir), startTestServerIn(t, dir)
	for _, c := range []*imapclient.Client{a, b} {
		if err := c.Login("user@test.com", "testpass").Wait(); err != nil {
			t.Fatal(err)
		}
	}
	appendMsg(t, a, "INBOX")
	appendMsg(t, a, "INBOX")
	if err := a.Create("Dest", nil).Wait(); err != nil {
		t.Fatal(err)
	}
	for _, c := range []*imapclient.Client{a, b} {
		if _, err := c.Select("INBOX", nil).Wait(); err != nil {
			t.Fatal(err)
		}
	}
	return a, b, dir
}

// dropListRow rewrites INBOX's uid list without uid's row, as a writer that
// rewrites the list after an expunge leaves it.
func dropListRow(t *testing.T, dir string, uid string) {
	t.Helper()
	var path string
	_ = filepath.Walk(dir, func(p string, info os.FileInfo, _ error) error {
		// INBOX's list sits above every other folder's.
		if info != nil && info.Name() == maildir.UIDListFileName && (path == "" || len(p) < len(path)) {
			path = p
		}
		return nil
	})
	raw, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	var kept []string
	for _, l := range strings.Split(string(raw), "\n") {
		if !strings.HasPrefix(l, uid+" ") {
			kept = append(kept, l)
		}
	}
	tmp := path + ".tmp"
	if err := os.WriteFile(tmp, []byte(strings.Join(kept, "\n")), 0o600); err != nil {
		t.Fatal(err)
	}
	if err := os.Rename(tmp, path); err != nil {
		t.Fatal(err)
	}
}

// A message another session expunges while this one reads it is gone, not
// unreadable: the unreadable count is the one an operator alerts on.
func TestAReadThatLostToAnExpungeIsCountedGone(t *testing.T) {
	a, b, dir := twoSessionsOnTwoMessages(t)
	defer expungeOnOpen(t, b, 1, 1, func() { dropListRow(t, dir, "1") })()
	gone, unr := unreadableByReason(t, "fetch", "gone"), unreadableByReason(t, "fetch", "unreadable")
	if _, err := a.Fetch(imaplib.SeqSetNum(1), &imaplib.FetchOptions{
		BodySection: []*imaplib.FetchItemBodySection{{Specifier: imaplib.PartSpecifierText, Peek: true}},
	}).Collect(); err != nil {
		t.Fatal(err)
	}
	if got := unreadableByReason(t, "fetch", "unreadable") - unr; got != 0 {
		t.Errorf("counted %v unreadable for a message another session expunged", got)
	}
	if got := unreadableByReason(t, "fetch", "gone") - gone; got != 1 {
		t.Errorf("counted %v gone, want 1", got)
	}
}

// A COPY that meets a message expunged under it answers EXPUNGEISSUED and
// leaves the destination as it was (RFC 2180 §4.4.1).
func TestACopyThatMeetsAnExpungeCopiesNothing(t *testing.T) {
	a, b, _ := twoSessionsOnTwoMessages(t)
	// The first open is message 1's, which is copied; message 2 goes under it.
	defer expungeOnOpen(t, b, 2, 2, nil)()
	_, err := a.Copy(imaplib.SeqSetNum(1, 2), "Dest").Wait()
	var imapErr *imaplib.Error
	if !errors.As(err, &imapErr) || imapErr.Code != imaplib.ResponseCode("EXPUNGEISSUED") {
		t.Fatalf("COPY answered %v, want NO [EXPUNGEISSUED]", err)
	}
	st, err := a.Status("Dest", &imaplib.StatusOptions{NumMessages: true}).Wait()
	if err != nil {
		t.Fatal(err)
	}
	if st.NumMessages == nil || *st.NumMessages != 0 {
		t.Errorf("Dest holds %d messages after a failed COPY, want 0", *st.NumMessages)
	}
}
