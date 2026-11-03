package main

import (
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/yarilomail/yarilo/v2/internal/backendapi"
	"github.com/yarilomail/yarilo/v2/internal/storage/index/file"
	"github.com/yarilomail/yarilo/v2/internal/storage/mailbox/mdbox"
	"github.com/yarilomail/yarilo/v2/internal/storage/mailboxbase"
	"github.com/yarilomail/yarilo/v2/pkg/mailbox"
)

// seedMdbox stores messages for liveUser the way a delivery does.
func seedMdbox(t *testing.T, be mailbox.MailboxBackend, resolver *mailbox.Resolver, msgs ...string) {
	t.Helper()
	info, err := resolver.UserInfo(liveUser, "")
	if err != nil {
		t.Fatal(err)
	}
	box := be.OpenUser(info)
	if err := box.Init(); err != nil {
		t.Fatal(err)
	}
	ui := file.New().OpenUser(info)
	f, err := ui.OpenFolder("INBOX", 0)
	if err != nil {
		t.Fatal(err)
	}
	for i, raw := range msgs {
		name, vsize, guid, err := box.Save("INBOX", strings.NewReader(raw), 1, int64(len(raw)), nil, nil, [16]byte{})
		if err != nil {
			t.Fatal(err)
		}
		meta := &mailbox.MessageMeta{UID: uint32(i + 1), Size: uint32(len(raw)), VSize: vsize, GUID: guid}
		if err := mailboxbase.NameSaved(box, "INBOX", name, meta); err != nil {
			t.Fatal(err)
		}
		if err := ui.AppendMessage(f.ID, meta); err != nil {
			t.Fatal(err)
		}
	}
	_ = ui.Close()
	_ = box.Close()
}

// purge and altmove run on an mdbox store: messages go to alt storage and back
// and stay readable, and a maildir account is told the verbs are mdbox's.
func TestMdboxVerbsAgainstTheBackend(t *testing.T) {
	be := mdbox.New()
	var resolver *mailbox.Resolver
	b := startLiveBackendWith(t, func(o *backendapi.Options) {
		o.Mailbox = be
		o.Resolver.DefaultAltDir = filepath.Join(o.Resolver.Root, "alt", "%d", "%n")
		resolver = o.Resolver
	})
	_ = b
	seedMdbox(t, be, resolver, liveMsg1, liveMsg2)

	var purge struct {
		FilesScanned    int `json:"files_scanned"`
		RecordsExpunged int `json:"records_expunged"`
	}
	decode(t, mustYarctl(t, "backend", "mdbox", "purge", liveUser), &purge)
	// Purge reads only files holding a record nothing points at.
	if purge.FilesScanned != 0 || purge.RecordsExpunged != 0 {
		t.Errorf("purge of a store with nothing expunged: %+v", purge)
	}

	var moved struct {
		Candidates int `json:"candidates"`
		Moved      int `json:"moved"`
	}
	// The cutoff reads the date the store recorded when the message was saved.
	decode(t, mustYarctl(t, "backend", "mdbox", "altmove", liveUser, "--before", "2000-01-01T00:00:00Z"), &moved)
	if moved.Moved != 0 {
		t.Errorf("altmove before 2000 moved %d messages saved today", moved.Moved)
	}
	decode(t, mustYarctl(t, "backend", "mdbox", "altmove", liveUser, "--before", time.Now().Add(time.Hour).Format(time.RFC3339)), &moved)
	if moved.Moved != 2 {
		t.Errorf("altmove before an hour from now: %+v, want both messages", moved)
	}
	decode(t, mustYarctl(t, "backend", "mdbox", "altmove", liveUser, "--reverse"), &moved)
	if moved.Moved != 2 {
		t.Errorf("altmove --reverse: %+v, want both messages back", moved)
	}
	var info struct {
		Messages int `json:"messages"`
	}
	decode(t, mustYarctl(t, "backend", "folder", "info", liveUser, "INBOX"), &info)
	if info.Messages != 2 {
		t.Errorf("INBOX after the moves holds %d messages, want 2", info.Messages)
	}

	if _, err := yarctl(t, "backend", "mdbox", "altmove", liveUser, "--before", "yesterday"); err == nil {
		t.Error("a cutoff that is not RFC 3339 was accepted")
	}
	for _, args := range [][]string{{"backend", "mdbox", "purge"}, {"backend", "mdbox", "altmove"}} {
		if _, err := yarctl(t, args...); err == nil || !strings.Contains(err.Error(), "usage") {
			t.Errorf("yarctl %s = %v, want its usage", strings.Join(args, " "), err)
		}
	}
	if _, err := yarctl(t, "backend", "mdbox", "nosuch"); err == nil {
		t.Error("an unknown mdbox command was accepted")
	}
}

// On a maildir account the mdbox verbs say they are mdbox's.
func TestMdboxVerbsRefuseOtherDrivers(t *testing.T) {
	b := startLiveBackend(t)
	seedInbox(t, b, map[string]string{"1700000000.M1P1.host:2,S": liveMsg1})
	for _, args := range [][]string{{"backend", "mdbox", "purge", liveUser}, {"backend", "mdbox", "altmove", liveUser}} {
		if _, err := yarctl(t, args...); err == nil || !strings.Contains(err.Error(), "mdbox") {
			t.Errorf("yarctl %s on maildir = %v, want the refusal naming mdbox", strings.Join(args, " "), err)
		}
	}
}
