package subs

import (
	"sort"
	"testing"

	"github.com/yarilomail/yarilo/v2/pkg/config"
	"github.com/yarilomail/yarilo/v2/pkg/mailbox"
)

func TestSubsAddSnapshotRemove(t *testing.T) {
	home := t.TempDir()
	store := New(home, "subscriptions", "alice@example.com", "test/1/alice@example.com", nil)

	cases := []struct {
		op       string
		folder   string
		expected []string
	}{
		{"add", "INBOX", []string{"INBOX"}},
		{"add", "Sent", []string{"INBOX", "Sent"}},
		{"add", "Sent", []string{"INBOX", "Sent"}}, // idempotent
		{"remove", "Sent", []string{"INBOX"}},
		{"remove", "Sent", []string{"INBOX"}}, // idempotent
	}
	for _, c := range cases {
		var err error
		switch c.op {
		case "add":
			err = store.Add(c.folder)
		case "remove":
			err = store.Remove(c.folder)
		}
		if err != nil {
			t.Fatalf("%s %s: %v", c.op, c.folder, err)
		}
		snap, err := store.Snapshot()
		if err != nil {
			t.Fatalf("snapshot after %s %s: %v", c.op, c.folder, err)
		}
		got := make([]string, 0, len(snap))
		for k := range snap {
			got = append(got, k)
		}
		sort.Strings(got)
		if !equalSlices(got, c.expected) {
			t.Errorf("after %s %s: got %v want %v", c.op, c.folder, got, c.expected)
		}
	}
}

func TestSubsSnapshotEmptyWhenMissing(t *testing.T) {
	home := t.TempDir()
	store := New(home, "subscriptions", "alice", "owner", nil)
	snap, err := store.Snapshot()
	if err != nil {
		t.Fatalf("snapshot: %v", err)
	}
	if len(snap) != 0 {
		t.Errorf("snapshot=%v want empty", snap)
	}
}

func equalSlices(a, b []string) bool {
	if len(a) != len(b) {
		return false
	}
	for i := range a {
		if a[i] != b[i] {
			return false
		}
	}
	return true
}

// A namespace that keeps its own subscriptions keeps them in its store; a
// shared store's file is locked under the store's name, not the user's (#2598).
func TestForNamespaceLocksASharedStoreByItsName(t *testing.T) {
	pub := t.TempDir()
	namespaces := []config.NamespaceConfig{
		{Type: "personal", Prefix: "", Separator: "/", Inbox: true},
		{Type: "shared", Prefix: "Public/", Separator: "/", Location: "maildir:" + pub},
	}
	personal := &mailbox.UserInfo{Username: "alice@example.com", Home: t.TempDir()}
	nsUI, err := mailbox.NamespaceUserInfo(personal, mailbox.Location{Driver: "maildir", Path: pub}, "/", mailbox.SharedByAll)
	if err != nil {
		t.Fatal(err)
	}
	store, _ := ForNamespace(namespaces, 1, nsUI, personal, "proc/1/alice@example.com/t", nil)
	if !namespaces[1].KeepsSubscriptions() {
		t.Skip("the namespace keeps no subscriptions of its own")
	}
	if got := store.lockUser(); got != "store:"+pub {
		t.Fatalf("lock name %q, want store:%s", got, pub)
	}
}
