package mailops

import (
	"errors"
	"io"
	"strings"
	"testing"

	"github.com/yarilomail/yarilo/v2/internal/storage/index/file"
	"github.com/yarilomail/yarilo/v2/internal/storage/mailbox/maildir"
	"github.com/yarilomail/yarilo/v2/internal/storage/mailboxbase"
	"github.com/yarilomail/yarilo/v2/pkg/mailbox"
)

// A move between stores copies each body and stops at the first one it cannot
// read, answering what landed so the caller expunges those and keeps the rest.
func TestAMoveBetweenStoresStopsAtTheFirstUnreadableBody(t *testing.T) {
	open := func(home string) mailbox.Box {
		info := &mailbox.UserInfo{Username: "u@example.com", Home: home, Driver: "maildir"}
		store := maildir.New().OpenUser(info)
		if err := store.Init(); err != nil {
			t.Fatal(err)
		}
		idx := file.New().OpenUser(info)
		t.Cleanup(func() { _ = store.Close(); _ = idx.Close() })
		return mailboxbase.Open(store, idx)
	}
	src, dst := open(t.TempDir()), open(t.TempDir())
	sf, err := src.Folder("INBOX", 0)
	if err != nil {
		t.Fatal(err)
	}
	df, err := dst.Folder("INBOX", 0)
	if err != nil {
		t.Fatal(err)
	}
	msgs := []*mailbox.MessageMeta{{UID: 1, GUID: [16]byte{1}}, {UID: 2, GUID: [16]byte{2}}, {UID: 3, GUID: [16]byte{3}}}
	unreadable := errors.New("body gone")
	o := New(Config{Protocol: "test"}, dst, &mailbox.UserInfo{Username: "u@example.com"})
	moved, err := o.MoveIn(MoveRequest{
		Src: src, Dst: dst, SrcFolder: sf, DstFolder: df, SrcName: "INBOX", DstName: "INBOX", Msgs: msgs,
		Read: func(m *mailbox.MessageMeta) (io.ReadCloser, error) {
			if m.UID == 2 {
				return nil, unreadable
			}
			return io.NopCloser(strings.NewReader("Subject: m\r\n\r\nbody\r\n")), nil
		},
	})
	if !errors.Is(err, unreadable) {
		t.Errorf("err %v, want the read failure", err)
	}
	if len(moved) != 1 || moved[0].Src.UID != 1 || moved[0].Dst.GUID != msgs[0].GUID {
		t.Errorf("moved %+v, want uid 1 alone with its GUID", moved)
	}
}
