package imap_test

import (
	"os"
	"path/filepath"
	"testing"

	imaplib "github.com/emersion/go-imap/v2"

	"github.com/yarilomail/yarilo/internal/userstate/acl"
)

// A MOVE into another namespace takes the source body with the record; a body
// left behind is an orphan the index no longer names.
func TestMoveIntoAnotherNamespaceLeavesNothingBehind(t *testing.T) {
	publicRoot, dial := publicNSServer(t)
	if err := os.WriteFile(filepath.Join(publicRoot, acl.RootFileName), []byte("user=alice lrwstipekxa\n"), 0o600); err != nil {
		t.Fatalf("seed root acl: %v", err)
	}
	a := dial("alice")
	if err := a.Create("Public/Box", nil).Wait(); err != nil {
		t.Fatalf("CREATE: %v", err)
	}
	body := "From: a@b\r\nSubject: moved\r\n\r\nx\r\n"
	cmd := a.Append("INBOX", int64(len(body)), nil)
	cmd.Write([]byte(body)) //nolint:errcheck
	cmd.Close()             //nolint:errcheck
	if _, err := cmd.Wait(); err != nil {
		t.Fatalf("APPEND: %v", err)
	}
	if _, err := a.Select("INBOX", nil).Wait(); err != nil {
		t.Fatalf("SELECT: %v", err)
	}
	if _, err := a.Move(imaplib.SeqSetNum(1), "Public/Box").Wait(); err != nil {
		t.Fatalf("MOVE: %v", err)
	}

	b := dial("alice")
	for _, folder := range []string{"INBOX", "Public/Box"} {
		want := map[string]uint32{"INBOX": 0, "Public/Box": 1}[folder]
		st, err := b.Status(folder, &imaplib.StatusOptions{NumMessages: true}).Wait()
		if err != nil {
			t.Fatalf("STATUS %s: %v", folder, err)
		}
		if st.NumMessages == nil || *st.NumMessages != want {
			t.Errorf("%s holds %v messages after the MOVE, want %d", folder, st.NumMessages, want)
		}
	}
	cur := filepath.Join(filepath.Dir(filepath.Dir(publicRoot)), "alice", "Maildir", "cur")
	if left, _ := os.ReadDir(cur); len(left) != 0 {
		t.Errorf("the source body stayed on disk after the MOVE: %s", left[0].Name())
	}
}
