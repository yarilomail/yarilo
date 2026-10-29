package mdbox

import (
	"errors"
	"os"
	"strings"
	"testing"
	"time"

	"github.com/yarilomail/yarilo/v2/internal/storage/mailbox/mdbox/mdboxmap"
)

// A rewrite whose map append fails after the new base landed keeps the new
// file: the map on disk names it, and deleting it would lose the mail.
func TestARewriteKeepsTheFileAMapThatLandedNames(t *testing.T) {
	// Armed once the new file is synced: the folds before the copy must land.
	failFold := func(t *testing.T) {
		armed := false
		prev := syncFile
		syncFile = func(f *os.File) error { armed = true; return prev(f) }
		t.Cleanup(func() { syncFile = prev })
		t.Cleanup(mdboxmap.SetTestLogRemoval(func(p string) error {
			if armed {
				return errors.New("log stayed")
			}
			return os.Remove(p)
		}))
	}
	t.Run("purge", func(t *testing.T) {
		home := t.TempDir()
		u := openTestUserMailbox(t, home)
		gone := testSaveMsg(t, u, "Subject: g\r\n\r\ngone\r\n")
		live := testSaveMsg(t, u, "Subject: l\r\n\r\nlive\r\n")
		if err := u.Remove("INBOX", gone); err != nil {
			t.Fatal(err)
		}
		failFold(t)
		if _, err := u.Purge(); err == nil {
			t.Fatal("purge reported success with the log left behind")
		}
		for who, box := range map[string]*userMailbox{"this process": u, "the next": openTestUserMailbox(t, home)} {
			if b := testFetchBody(t, box, live); !strings.Contains(b, "live") {
				t.Errorf("%s reads %q", who, b)
			}
		}
	})
	t.Run("alt move", func(t *testing.T) {
		_, u, _ := altTestBackend(t)
		old := saveReceivedAt(t, u, "Subject: o\r\n\r\nold\r\n", time.Now().Add(-48*time.Hour))
		fresh := saveReceivedAt(t, u, "Subject: n\r\n\r\nnew\r\n", time.Now())
		failFold(t)
		if _, err := u.AltMove(AltMoveQuery{Before: time.Now().Add(-time.Hour)}); err == nil {
			t.Fatal("alt move reported success with the log left behind")
		}
		for name, want := range map[string]string{old: "old", fresh: "new"} {
			if b := testFetchBody(t, u, name); !strings.Contains(b, want) {
				t.Errorf("%s after the failed fold: %q", want, b)
			}
		}
	})
}
