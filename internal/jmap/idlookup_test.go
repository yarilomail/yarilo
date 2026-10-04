package jmap

import (
	"encoding/hex"
	"net"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/prometheus/client_golang/prometheus/testutil"

	"github.com/yarilomail/yarilo/internal/storage/index/file"
	"github.com/yarilomail/yarilo/internal/storage/mailbox/maildir"
	"github.com/yarilomail/yarilo/internal/storage/mailboxbase"
	"github.com/yarilomail/yarilo/pkg/mailbox"
)

// lookupServer delivers one message into each of several folders, through the
// transaction that records copies, and returns the server and the ids.
func lookupServer(t *testing.T, folders ...string) (*Server, []string) {
	t.Helper()
	home := t.TempDir()
	info := &mailbox.UserInfo{Username: testUser, Home: home, Separator: "/"}
	locker := &testLocker{}
	box := maildir.New().OpenUser(info)
	t.Cleanup(func() { box.Close() }) //nolint:errcheck
	if err := box.Init(); err != nil {
		t.Fatal(err)
	}
	ui := file.New(file.WithLocker(locker)).OpenUser(info)

	var ids []string
	for _, name := range folders {
		if name != "INBOX" {
			if err := box.Create(name); err != nil {
				t.Fatal(err)
			}
		}
		// Three per folder, so reading the folder is not the same as reading
		// the record: a row over one message cannot tell them apart.
		var first [16]byte
		for i := 0; i < 3; i++ {
			raw := "Subject: " + name + "\r\n\r\nbody\r\n"
			saved, vsize, guid, err := box.Save(name, strings.NewReader(raw), 1, int64(len(raw)), []string{`\Seen`}, nil, [16]byte{})
			if err != nil {
				t.Fatal(err)
			}
			f, err := ui.OpenFolder(name, 0)
			if err != nil {
				t.Fatal(err)
			}
			meta := &mailbox.MessageMeta{
				Size: uint32(len(raw)), VSize: vsize, Flags: []string{`\Seen`},
				GUID: guid, InternalDate: time.Now(),
			}
			tx, err := ui.Begin(f.ID)
			if err != nil {
				t.Fatal(err)
			}
			tx.Append(meta)
			if _, err := tx.Commit(); err != nil {
				t.Fatal(err)
			}
			if err := mailboxbase.NameSaved(box, name, saved, meta); err != nil {
				t.Fatal(err)
			}
			if i == 0 {
				first = guid
			}
		}
		guid := first
		ids = append(ids, hex.EncodeToString(guid[:]))
	}
	if err := ui.Close(); err != nil {
		t.Fatal(err)
	}

	s := New(Options{
		Trust:  ResolveTrust(false, true, []*net.IPNet{mustCIDR(t, "192.0.2.0/24")}),
		Limits: testLimits(),
		Storage: &Storage{
			Mailbox:     maildir.New(),
			Index:       file.New(file.WithLocker(locker)),
			ResolveUser: func(string) (*mailbox.UserInfo, error) { return info, nil },
			Locker:      locker,
		},
	})
	return s, ids
}

// The datum this arc was asked for: what one id costs. Through the store it is
// one folder and one record; the walk it replaces reads every folder (#1711).
func TestAnIDCostsOneFolderThroughTheStore(t *testing.T) {
	s, ids := lookupServer(t, "INBOX", "Archive", "Sent", "Drafts")
	h, err := s.opts.Storage.open(testUser, "")
	if err != nil {
		t.Fatal(err)
	}
	want := map[string]bool{ids[len(ids)-1]: true}

	folders0 := testutil.ToFloat64(metricIDLookupFolders)
	records0 := testutil.ToFloat64(metricIDLookupRecords)
	walk, err := s.findByWalk(h, want)
	if err != nil {
		t.Fatal(err)
	}
	walkFolders := testutil.ToFloat64(metricIDLookupFolders) - folders0
	walkRecords := testutil.ToFloat64(metricIDLookupRecords) - records0

	folders1 := testutil.ToFloat64(metricIDLookupFolders)
	records1 := testutil.ToFloat64(metricIDLookupRecords)
	found, ok, err := s.findThroughStore(h, want)
	if err != nil {
		t.Fatal(err)
	}
	if !ok {
		t.Fatal("the store did not answer, so the copies a transaction wrote are not there")
	}
	storeFolders := testutil.ToFloat64(metricIDLookupFolders) - folders1
	storeRecords := testutil.ToFloat64(metricIDLookupRecords) - records1

	t.Logf("one id: walk opened %v folders and read %v records; the store opened %v and read %v",
		walkFolders, walkRecords, storeFolders, storeRecords)
	if len(found) != 1 || found[ids[len(ids)-1]].meta == nil {
		t.Fatalf("the store answered %v, one id was asked for", found)
	}
	if walk[ids[len(ids)-1]].folder != found[ids[len(ids)-1]].folder {
		t.Errorf("the store says %q, the walk says %q",
			found[ids[len(ids)-1]].folder, walk[ids[len(ids)-1]].folder)
	}
	if storeFolders != 1 {
		t.Errorf("the store opened %v folders for one id, want 1", storeFolders)
	}
	if storeRecords != 1 {
		t.Errorf("the store read %v records for one id, want 1 (the walk read %v)", storeRecords, walkRecords)
	}
}

// A store that says nothing sends the request back to the walk: the file is
// derived, so an account that has none still answers.
func TestAnUnrecordedMailboxFallsBackToTheWalk(t *testing.T) {
	s, id, home := storedServerWithMessageAt(t, "Subject: x\r\n\r\nbody\r\n", 0)
	for _, name := range []string{file.GUIDIndexFileName, file.GUIDIndexFileName + ".log"} {
		if err := os.Remove(filepath.Join(home, name)); err != nil && !os.IsNotExist(err) {
			t.Fatal(err)
		}
	}
	h, err := s.opts.Storage.open(testUser, "")
	if err != nil {
		t.Fatal(err)
	}
	want := map[string]bool{id: true}
	if _, ok, err := s.findThroughStore(h, want); err != nil || ok {
		t.Fatalf("the store answered for a mailbox that never recorded a copy: ok=%v err=%v", ok, err)
	}
	found, err := s.findMessages(h, want)
	if err != nil {
		t.Fatal(err)
	}
	if len(found) != 1 {
		t.Errorf("the walk found %d messages, one was asked for", len(found))
	}
}
