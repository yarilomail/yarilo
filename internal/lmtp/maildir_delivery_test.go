package lmtp

import (
	"io/fs"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/yarilomail/yarilo/v2/internal/storage/mailbox/maildir"
	"github.com/yarilomail/yarilo/v2/internal/storage/mailboxbase"
	"github.com/yarilomail/yarilo/v2/internal/storage/mailops"
	"github.com/yarilomail/yarilo/v2/pkg/mailbox"
)

// A delivery into maildir names the file it wrote itself, so it lists no cur/
// (#2639), and its uid is one line at the end of the list, not the list
// written again (#2640): the list is the same file afterwards, one row longer.
func TestAMaildirDeliveryListsNothingAndAppendsItsRow(t *testing.T) {
	d := costDrivers[0]
	resolver := &mailbox.Resolver{Root: t.TempDir(), HomeTemplate: "%d/%n", DefaultQuotaRules: []string{"*:storage=10G"}}
	info, err := resolver.UserInfo("alice@example.com", "")
	if err != nil {
		t.Fatal(err)
	}
	info.Driver = d.name
	fill := &session{opts: costOptions(d, resolver)}
	box := fill.opts.Mailbox.OpenUser(info)
	if err := box.Init(); err != nil {
		t.Fatal(err)
	}
	ui := fill.opts.Index.OpenUser(info)
	mbox := mailboxbase.Open(box, ui)
	ops := mailops.NewDelivery(fill.mailopsConfig(), mbox, info)
	for i := range 50 {
		raw := costMessage(i)
		if _, _, _, err := deliverOne(ops, mbox, "INBOX", strings.NewReader(raw), int64(len(raw)), info.Username, "s@x", nil); err != nil {
			t.Fatal(err)
		}
	}
	box.Close() //nolint:errcheck
	ui.Close()  //nolint:errcheck

	list := uidListOf(t, info.Home)
	before, err := os.Stat(list)
	if err != nil {
		t.Fatal(err)
	}
	rows := strings.Count(readFile(t, list), "\n")
	reads := maildir.DirReads()

	s := &session{opts: costOptions(d, resolver), from: "s@x", rcpts: []string{info.Username},
		rcptUserInfo: map[string]*mailbox.UserInfo{info.Username: info}}
	sink := &statusSink{}
	if err := s.LMTPData(strings.NewReader(costMessage(50)), sink); err != nil || sink.errs[info.Username] != nil {
		t.Fatalf("deliver: %v %v", err, sink.errs[info.Username])
	}

	if got := maildir.DirReads() - reads; got != 0 {
		t.Errorf("the delivery listed cur/ %d times", got)
	}
	after, err := os.Stat(list)
	if err != nil {
		t.Fatal(err)
	}
	if !os.SameFile(before, after) {
		t.Error("the delivery wrote the list again instead of appending its row")
	}
	if got := strings.Count(readFile(t, list), "\n"); got != rows+1 {
		t.Errorf("the list has %d rows after the delivery, want %d", got, rows+1)
	}
	// And the record still leads to the file.
	check := s.opts.Index.OpenUser(info)
	defer check.Close() //nolint:errcheck
	rbox := s.opts.Mailbox.OpenUser(info)
	defer rbox.Close() //nolint:errcheck
	f, err := check.OpenFolder("INBOX", 0)
	if err != nil {
		t.Fatal(err)
	}
	msgs, err := check.GetMessages(f.ID, mailbox.SeqSet{{From: f.NextUID - 1, To: f.NextUID - 1}})
	if err != nil || len(msgs) != 1 {
		t.Fatalf("the delivered record: %v, %d", err, len(msgs))
	}
	rc, err := mailboxbase.OpenMessage(rbox, "INBOX", msgs[0])
	if err != nil {
		t.Fatalf("the delivered message cannot be read from its record: %v", err)
	}
	rc.Close() //nolint:errcheck
}

func uidListOf(t *testing.T, home string) string {
	t.Helper()
	var found string
	_ = filepath.WalkDir(home, func(p string, e fs.DirEntry, _ error) error {
		// INBOX's list: every other folder's sits in its own ".Name" directory.
		if e != nil && !e.IsDir() && e.Name() == maildir.UIDListFileName && !strings.HasPrefix(filepath.Base(filepath.Dir(p)), ".") && found == "" {
			found = p
		}
		return nil
	})
	if found == "" {
		t.Fatal("no uid list under the home")
	}
	return found
}

func readFile(t *testing.T, p string) string {
	t.Helper()
	b, err := os.ReadFile(p)
	if err != nil {
		t.Fatal(err)
	}
	return string(b)
}
