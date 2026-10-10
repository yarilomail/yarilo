package maildir

import (
	"os"
	"os/exec"
	"strings"
	"syscall"
	"testing"

	fileidx "github.com/yarilomail/yarilo/internal/storage/index/file"
	"github.com/yarilomail/yarilo/internal/storage/mailboxbase"
	"github.com/yarilomail/yarilo/pkg/mailbox"
)

// reconcileWindowChildHome, set, makes this test the delivering process.
const reconcileWindowChildHome = "YARILO_RECONCILE_WINDOW_CHILD_HOME"

func openWindowUser(t *testing.T, home string) (*userMailbox, mailbox.UserIndex) {
	t.Helper()
	info := &mailbox.UserInfo{Username: "u@x.com", Home: home}
	box := New().OpenUser(info).(*userMailbox)
	t.Cleanup(func() { box.Close() }) //nolint:errcheck
	idx := fileidx.New().OpenUser(info)
	t.Cleanup(func() { idx.Close() }) //nolint:errcheck
	return box, idx
}

func deliverOne(t *testing.T, box *userMailbox, idx mailbox.UserIndex, folder *mailbox.Folder) {
	t.Helper()
	const body = "From: a@b\r\n\r\nx\r\n"
	saved, vsize, guid, err := box.Save("INBOX", strings.NewReader(body), 0, int64(len(body)), []string{`\Seen`}, nil, [16]byte{})
	if err != nil {
		t.Fatal(err)
	}
	m := &mailbox.MessageMeta{Size: uint32(len(body)), VSize: vsize, GUID: guid, Flags: []string{`\Seen`}}
	if err := mailboxbase.RecordSaved(idx, box, folder.ID, "INBOX", saved, m); err != nil {
		t.Fatal(err)
	}
}

func listInode(t *testing.T, box *userMailbox) uint64 {
	t.Helper()
	fi, err := os.Stat(box.uidListPath("INBOX"))
	if err != nil {
		t.Fatal(err)
	}
	return fi.Sys().(*syscall.Stat_t).Ino
}

// Another process delivers between two reconciles of this one, as LMTP does
// beside IMAP. The second pass must see the row that process wrote: a stale
// map rewrites the list for it or stops on the uid the index holds (#2181).
func TestAReconcileSeesRowsAnotherProcessWrote(t *testing.T) {
	if home := os.Getenv(reconcileWindowChildHome); home != "" {
		box, idx := openWindowUser(t, home)
		folder, err := idx.OpenFolder("INBOX", 0)
		if err != nil {
			t.Fatal(err)
		}
		deliverOne(t, box, idx, folder)
		return
	}
	root := t.TempDir()
	home := testHome(root, "u@x.com")
	box, idx := openWindowUser(t, home)
	if err := box.Init(); err != nil {
		t.Fatal(err)
	}
	if err := box.Create("INBOX"); err != nil {
		t.Fatal(err)
	}
	folder, err := idx.OpenFolder("INBOX", 1)
	if err != nil {
		t.Fatal(err)
	}
	deliverOne(t, box, idx, folder)
	if _, err := box.ReconcileIndex(mailboxbase.Open(box, idx), idx, folder); err != nil {
		t.Fatal(err)
	}

	child := exec.Command(os.Args[0], "-test.run=^TestAReconcileSeesRowsAnotherProcessWrote$", "-test.count=1")
	child.Env = append(os.Environ(), reconcileWindowChildHome+"="+home)
	if out, err := child.CombinedOutput(); err != nil {
		t.Fatalf("the delivering process failed: %v\n%s", err, out)
	}

	before := listInode(t, box)
	folder, err = idx.OpenFolder("INBOX", folder.UIDValidity)
	if err != nil {
		t.Fatal(err)
	}
	st, err := box.ReconcileIndex(mailboxbase.Open(box, idx), idx, folder)
	if err != nil {
		t.Fatalf("the pass stopped: %v", err)
	}
	if st.Relinked != 0 || st.Imported != 0 || st.Expunged != 0 {
		t.Errorf("the pass changed %+v for a delivery it should simply see", st)
	}
	if listInode(t, box) != before {
		t.Error("the pass rewrote the list for a row that was never lost")
	}
	msgs, err := idx.GetMessages(folder.ID, mailbox.SeqSet{{From: 1, To: 0}})
	if err != nil || len(msgs) != 2 {
		t.Errorf("index = %d records, err = %v; want 2", len(msgs), err)
	}
}
