package jmap

import (
	"encoding/hex"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	fileindex "github.com/yarilomail/yarilo/internal/storage/index/file"
	"github.com/yarilomail/yarilo/internal/storage/mailbox/maildir"
	"github.com/yarilomail/yarilo/internal/storage/mailboxbase"
	"github.com/yarilomail/yarilo/pkg/mailbox"
)

// copyInto puts the message into a second folder under its own identity, the
// way COPY does (RFC 8474 §5.1), and returns that folder's name.
func copyInto(t *testing.T, home, folder, id string) {
	t.Helper()
	info := &mailbox.UserInfo{Username: testUser, Home: home, Separator: "/"}
	box := maildir.New().OpenUser(info)
	ui := fileindex.New().OpenUser(info)
	t.Cleanup(func() { box.Close(); ui.Close() }) //nolint:errcheck
	if err := box.Create(folder); err != nil {
		t.Fatal(err)
	}
	raw := "Subject: x\r\n\r\nbody\r\n"
	guidBytes, err := hex.DecodeString(id)
	if err != nil {
		t.Fatal(err)
	}
	var guid [16]byte
	copy(guid[:], guidBytes)
	name, vsize, saved, err := box.Save(folder, strings.NewReader(raw), 1, int64(len(raw)), []string{`\Seen`}, nil, guid)
	if err != nil {
		t.Fatal(err)
	}
	if saved != guid {
		t.Fatalf("the copy was stored under %x, not the source's identity", saved)
	}
	meta := &mailbox.MessageMeta{UID: 1, Size: uint32(len(raw)), VSize: vsize,
		Flags: []string{`\Seen`}, GUID: guid, InternalDate: time.Now()}
	if err := mailboxbase.NameSaved(box, folder, name, meta); err != nil {
		t.Fatal(err)
	}
	f, err := ui.OpenFolder(folder, 0)
	if err != nil {
		t.Fatal(err)
	}
	if err := ui.AppendMessage(f.ID, meta); err != nil {
		t.Fatal(err)
	}
}

// One message in two mailboxes is one Email: the id appears once in a query,
// and the mailboxes are a property of it (RFC 8621 §4).
func TestQueryFoldsTheCopiesOfOneMessage(t *testing.T) {
	s, id, home := storedServerWithMessageAt(t, "Subject: x\r\n\r\nbody\r\n", 0)
	copyInto(t, home, "Archive", id)

	ids := idsOf(t, emailQuery(t, s, `{"accountId":"`+testUser+`"}`))
	if len(ids) != 1 || ids[0] != id {
		t.Fatalf("Email/query returned %v for one message in two mailboxes, want [%s]", ids, id)
	}
}

// Email/get answers where the message is, not where it was found.
func TestGetReportsEveryMailboxHoldingTheMessage(t *testing.T) {
	s, id, home := storedServerWithMessageAt(t, "Subject: x\r\n\r\nbody\r\n", 0)
	copyInto(t, home, "Archive", id)

	got := callAPI(t, s, `{"using":["urn:ietf:params:jmap:mail"],"methodCalls":[
		["Email/get",{"accountId":"`+testUser+`","ids":["`+id+`"],"properties":["id","mailboxIds","keywords"]},"c0"]]}`)
	list, _ := got["list"].([]any)
	if len(list) != 1 {
		t.Fatalf("Email/get returned %d objects for one message", len(list))
	}
	email, _ := list[0].(map[string]any)
	boxes, _ := email["mailboxIds"].(map[string]any)
	if len(boxes) != 2 {
		t.Errorf("mailboxIds names %d mailboxes, want both that hold a copy: %v", len(boxes), boxes)
	}
}

// A keyword set through JMAP reaches every copy, or the union Email/get
// reports would show a flag the client cannot remove.
func TestSetWritesTheKeywordToEveryCopy(t *testing.T) {
	s, id, home := storedServerWithMessageAt(t, "Subject: x\r\n\r\nbody\r\n", 0)
	copyInto(t, home, "Archive", id)

	callAPI(t, s, `{"using":["urn:ietf:params:jmap:mail"],"methodCalls":[
		["Email/set",{"accountId":"`+testUser+`","update":{"`+id+`":{"keywords/$flagged":true}}},"c0"]]}`)

	info := &mailbox.UserInfo{Username: testUser, Home: home, Separator: "/"}
	box := maildir.New().OpenUser(info)
	ui := fileindex.New().OpenUser(info)
	defer func() { box.Close(); ui.Close() }() //nolint:errcheck
	mb := mailboxbase.Open(box, ui)
	for _, folder := range []string{"INBOX", "Archive"} {
		f, err := mb.Folder(folder, 0)
		if err != nil {
			t.Fatal(err)
		}
		metas, err := mb.Messages(f.ID, mailbox.SeqSet{{From: 1, To: 0}})
		if err != nil || len(metas) == 0 {
			t.Fatalf("%s: %v", folder, err)
		}
		if !hasFlag(metas[0].Flags, `\Flagged`) {
			t.Errorf("the copy in %s carries %v, without the keyword the client set", folder, metas[0].Flags)
		}
	}
}

// The store is derived: an account that has none answers the same, through the
// walk that id lookup already falls back to.
func TestTheAnswersHoldWithoutTheGUIDStore(t *testing.T) {
	s, id, home := storedServerWithMessageAt(t, "Subject: x\r\n\r\nbody\r\n", 0)
	copyInto(t, home, "Archive", id)
	for _, name := range []string{fileindex.GUIDIndexFileName, fileindex.GUIDIndexFileName + ".log"} {
		if err := os.Remove(filepath.Join(home, name)); err != nil && !os.IsNotExist(err) {
			t.Fatal(err)
		}
	}

	ids := idsOf(t, emailQuery(t, s, `{"accountId":"`+testUser+`"}`))
	if len(ids) != 1 {
		t.Errorf("Email/query returned %v without the store, want one id", ids)
	}
	got := callAPI(t, s, `{"using":["urn:ietf:params:jmap:mail"],"methodCalls":[
		["Email/get",{"accountId":"`+testUser+`","ids":["`+id+`"],"properties":["id","mailboxIds"]},"c0"]]}`)
	list, _ := got["list"].([]any)
	if len(list) != 1 {
		t.Fatalf("Email/get returned %d objects without the store", len(list))
	}
	email, _ := list[0].(map[string]any)
	boxes, _ := email["mailboxIds"].(map[string]any)
	if len(boxes) != 2 {
		t.Errorf("mailboxIds names %d mailboxes without the store, want 2", len(boxes))
	}
}

func hasFlag(flags []string, want string) bool {
	for _, f := range flags {
		if f == want {
			return true
		}
	}
	return false
}
