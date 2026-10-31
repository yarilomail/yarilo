package file

import (
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/yarilomail/yarilo/v2/pkg/locks/lockstest"
	"github.com/yarilomail/yarilo/v2/pkg/mailbox"
)

// With VOLATILEDIR apart, a new journal is staged there and lands beside the
// index: the next process reads it, and no scratch copy is left anywhere.
func TestAJournalStagedInTheVolatileDirLandsBesideTheIndex(t *testing.T) {
	dial := lockstest.Server(t)
	home, vdir := t.TempDir(), filepath.Join(t.TempDir(), "volatile")
	open := func() mailbox.UserIndex {
		idx := New(WithLocker(dial())).OpenUser(&mailbox.UserInfo{Username: "alice@example.com", Home: home, VolatileDir: vdir})
		t.Cleanup(func() { _ = idx.Close() })
		return idx
	}
	first := open()
	f := inbox(t, first)
	must(t, first.AppendMessage(f.ID, &mailbox.MessageMeta{UID: 1, Size: 10}))

	second := open()
	if n := len(allOf(t, second, inbox(t, second).ID)); n != 1 {
		t.Errorf("the next process reads %d messages, want 1", n)
	}
	var logs []string
	_ = filepath.WalkDir(home, func(p string, d os.DirEntry, _ error) error {
		if d != nil && strings.HasSuffix(d.Name(), ".log") {
			logs = append(logs, p)
		}
		return nil
	})
	if len(logs) == 0 {
		t.Error("no journal beside the index")
	}
	for _, dir := range []string{home, vdir} {
		_ = filepath.WalkDir(dir, func(p string, d os.DirEntry, _ error) error {
			if d != nil && (strings.Contains(d.Name(), ".tmp.") || strings.Contains(d.Name(), ".stage.")) {
				t.Errorf("scratch copy left behind: %s", p)
			}
			return nil
		})
	}
}
