package mailops

import (
	"context"
	"errors"
	"fmt"
	"slices"
	"sync"
	"testing"

	"github.com/yarilomail/yarilo/v2/internal/mailboxcreate"
	"github.com/yarilomail/yarilo/v2/internal/storage/index/file"
	"github.com/yarilomail/yarilo/v2/internal/storage/mailbox/maildir"
	"github.com/yarilomail/yarilo/v2/internal/storage/mailboxbase"
	"github.com/yarilomail/yarilo/v2/internal/userstate/acl"
	"github.com/yarilomail/yarilo/v2/pkg/locks"
	"github.com/yarilomail/yarilo/v2/pkg/mailbox"
	"github.com/yarilomail/yarilo/v2/pkg/quota"
)

// emitRecorder is a Locker that records events and serves nothing else.
type emitRecorder struct {
	locks.Locker
	mu     sync.Mutex
	events []string
}

func (r *emitRecorder) Emit(_ context.Context, resource string, t locks.EventType, payload string) error {
	r.mu.Lock()
	defer r.mu.Unlock()
	r.events = append(r.events, resource+" "+string(t)+" "+payload)
	return nil
}

func folderOps(t *testing.T, mailboxCount int64) (*Ops, mailbox.Box, *emitRecorder, *mailbox.UserInfo) {
	t.Helper()
	info := &mailbox.UserInfo{Username: "u@example.com", Home: t.TempDir(), Driver: "maildir"}
	store := maildir.New().OpenUser(info)
	idx := file.New().OpenUser(info)
	t.Cleanup(func() { _ = store.Close(); _ = idx.Close() })
	if err := store.Init(); err != nil {
		t.Fatal(err)
	}
	rec := &emitRecorder{}
	box := mailboxbase.Open(store, idx)
	cfg := Config{Protocol: "test", Locker: rec, Quota: QuotaConfig{Policy: quota.Policy{MailboxCount: mailboxCount}}}
	return New(cfg, box, info), box, rec, info
}

func TestFolderOperationsAnnounceWhatTheyDid(t *testing.T) {
	o, box, rec, _ := folderOps(t, 0)
	list := locks.MailboxListKey("u@example.com")
	if err := o.CreateFolder(box, "A", "A"); err != nil {
		t.Fatalf("create: %v", err)
	}
	if err := o.RenameFolder(box, nil, "A", "B", "A", "B"); err != nil {
		t.Fatalf("rename: %v", err)
	}
	if err := o.DeleteFolder(box, nil, "B", "B"); err != nil {
		t.Fatalf("delete: %v", err)
	}
	o.Subscribed("B", false)
	want := []string{
		list + " " + string(locks.EventMailboxCreate) + " A",
		list + " " + string(locks.EventMailboxRename) + " A" + RenameSeparator + "B",
		list + " " + string(locks.EventMailboxDelete) + " B",
		list + " " + string(locks.EventMailboxUnsubscribe) + " B",
	}
	if !slices.Equal(rec.events, want) {
		t.Errorf("events %q, want %q", rec.events, want)
	}
}

// Every creation is held to quota_mailbox_count, and a refused one is silent.
func TestCreateFolderHoldsTheMailboxCount(t *testing.T) {
	o, box, rec, _ := folderOps(t, 2)
	if err := o.CreateFolder(box, "A", "A"); err != nil {
		t.Fatalf("the second folder was refused: %v", err)
	}
	if err := o.CreateFolder(box, "B", "B"); !errors.Is(err, mailboxcreate.ErrLimit) {
		t.Errorf("the third folder answered %v, want ErrLimit", err)
	}
	if len(rec.events) != 1 {
		t.Errorf("events %q, want only A's", rec.events)
	}
}

// A folder's ACL follows it in the namespace's ACL index: renamed with the
// folder, dropped with it. The file itself lives in the folder and moves anyway.
func TestFolderACLFollowsTheFolder(t *testing.T) {
	o, box, _, info := folderOps(t, 0)
	store := acl.New(info.Home, info.MailPath, info.Driver, "/", "", info.Username, "test", acl.Policy{}, nil)
	if err := o.CreateFolder(box, "A", "A"); err != nil {
		t.Fatal(err)
	}
	bob := mailbox.Identifier{Type: mailbox.IDUser, Name: "bob"}
	if err := store.Set("A", mailbox.ACL{{Identifier: bob, Rights: "lr"}}); err != nil {
		t.Fatal(err)
	}
	indexed := func() []string {
		entries, err := store.ListSnapshot()
		if err != nil {
			t.Fatal(err)
		}
		var names []string
		for _, e := range entries {
			names = append(names, e.Mailbox)
		}
		return names
	}
	if err := o.RenameFolder(box, store, "A", "B", "A", "B"); err != nil {
		t.Fatal(err)
	}
	if got := indexed(); fmt.Sprint(got) != "[B]" {
		t.Errorf("after rename the index names %v, want [B]", got)
	}
	if err := o.DeleteFolder(box, store, "B", "B"); err != nil {
		t.Fatal(err)
	}
	if got := indexed(); len(got) != 0 {
		t.Errorf("after delete the index names %v", got)
	}
}
