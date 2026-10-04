package integration_test

import (
	"bytes"
	"io"
	"os"
	"path/filepath"
	"sort"
	"strings"
	"testing"

	indexfile "github.com/yarilomail/yarilo/internal/storage/index/file"
	"github.com/yarilomail/yarilo/internal/storage/mailbox/dboxref"
	"github.com/yarilomail/yarilo/internal/storage/mailbox/dboxv2"
	"github.com/yarilomail/yarilo/internal/storage/mailbox/mdbox"
	"github.com/yarilomail/yarilo/internal/storage/mailboxbase"
	"github.com/yarilomail/yarilo/pkg/mailbox"
)

// A message is found from its uid alone, across a reopen: what the folder
// records is enough, and no sidecar takes part (#1700).
func TestAMessageIsFoundByItsUIDAfterAReopen(t *testing.T) {
	for _, tc := range []struct {
		driver string
		open   func(*mailbox.UserInfo) mailbox.UserMailbox
	}{
		{"sdbox", func(i *mailbox.UserInfo) mailbox.UserMailbox { return dboxv2.New().OpenUser(i) }},
		{"mdbox", func(i *mailbox.UserInfo) mailbox.UserMailbox { return mdbox.New().OpenUser(i) }},
	} {
		t.Run(tc.driver, func(t *testing.T) {
			home := t.TempDir()
			info := &mailbox.UserInfo{Username: "u1@example.com", Home: home, Driver: tc.driver}
			box := tc.open(info)
			if err := box.Init(); err != nil {
				t.Fatal(err)
			}
			idx := indexfile.New().OpenUser(info)
			f, err := idx.OpenFolder("INBOX", 1)
			if err != nil {
				t.Fatal(err)
			}
			body := "From: a@b\r\n\r\n" + tc.driver + " body\r\n"
			saved, vsize, guid, err := box.Save("INBOX", strings.NewReader(body), 0, int64(len(body)), nil, nil, [16]byte{})
			if err != nil {
				t.Fatal(err)
			}
			m := &mailbox.MessageMeta{Size: uint32(len(body)), VSize: vsize, GUID: guid}
			if err := mailboxbase.RecordSaved(idx, box, f.ID, "INBOX", saved, m); err != nil {
				t.Fatal(err)
			}
			uid := m.UID
			box.Close() //nolint:errcheck
			idx.Close() //nolint:errcheck

			// A fresh view of the same store, holding nothing from the save.
			box2 := tc.open(info)
			defer box2.Close() //nolint:errcheck
			if _, ok := mailbox.Driver(box2).(mailbox.UIDAddressable); !ok {
				t.Fatalf("the %s driver cannot find a message from its record", tc.driver)
			}
			idx2 := indexfile.New().OpenUser(info)
			defer idx2.Close() //nolint:errcheck
			f2, err := idx2.OpenFolder("INBOX", 0)
			if err != nil {
				t.Fatal(err)
			}
			msgs, err := idx2.GetMessages(f2.ID, mailbox.SeqSet{{From: 1, To: 0}})
			if err != nil {
				t.Fatal(err)
			}
			if len(msgs) != 1 || msgs[0].UID != uid {
				t.Fatalf("the folder reads back as %v", msgs)
			}
			rc, err := mailboxbase.OpenMessage(box2, "INBOX", msgs[0])
			if err != nil {
				t.Fatalf("open uid %d: %v", uid, err)
			}
			defer rc.Close() //nolint:errcheck
			got, err := io.ReadAll(rc)
			if err != nil {
				t.Fatal(err)
			}
			if string(got) != body {
				t.Errorf("uid %d reads %q, want %q", uid, got, body)
			}
		})
	}
}

// Reading a message by uid costs the folder read and nothing else: no listing
// of the directory, and no walk of the map (#1700).
func TestReadingByUIDWalksNothing(t *testing.T) {
	home := t.TempDir()
	info := &mailbox.UserInfo{Username: "u1@example.com", Home: home, Driver: "mdbox"}
	box := mdbox.New().OpenUser(info)
	defer box.Close() //nolint:errcheck
	if err := box.Init(); err != nil {
		t.Fatal(err)
	}
	idx := indexfile.New().OpenUser(info)
	defer idx.Close() //nolint:errcheck
	f, err := idx.OpenFolder("INBOX", 1)
	if err != nil {
		t.Fatal(err)
	}
	body := "From: a@b\r\n\r\nx\r\n"
	for i := 0; i < 5; i++ {
		saved, vsize, guid, serr := box.Save("INBOX", strings.NewReader(body), 0, int64(len(body)), nil, nil, [16]byte{})
		if serr != nil {
			t.Fatal(serr)
		}
		m := &mailbox.MessageMeta{Size: uint32(len(body)), VSize: vsize, GUID: guid}
		if err := mailboxbase.RecordSaved(idx, box, f.ID, "INBOX", saved, m); err != nil {
			t.Fatal(err)
		}
		_ = m
	}

	dirReads, mapWalks := mdbox.SetTestCounters()
	defer mdbox.SetTestCounters() //nolint:errcheck
	msgs, err := idx.GetMessages(f.ID, mailbox.SeqSet{{From: 1, To: 0}})
	if err != nil {
		t.Fatal(err)
	}
	for _, m := range msgs {
		rc, oerr := mailboxbase.OpenMessage(box, "INBOX", m)
		if oerr != nil {
			t.Fatalf("uid %d: %v", m.UID, oerr)
		}
		rc.Close() //nolint:errcheck
	}
	if got := dirReads(); got != 0 {
		t.Errorf("reading %d messages listed the folder %d times", len(msgs), got)
	}
	if got := mapWalks(); got != 0 {
		t.Errorf("reading %d messages walked the map %d times", len(msgs), got)
	}
}

// The size survives a reopen: it used to live in the sidecar's third column,
// and RFC822.SIZE and POP3 LIST answer from it (#1700).
func TestTheSizeSurvivesAReopen(t *testing.T) {
	for _, tc := range []struct {
		driver string
		open   func(*mailbox.UserInfo) mailbox.UserMailbox
	}{
		{"sdbox", func(i *mailbox.UserInfo) mailbox.UserMailbox { return dboxv2.New().OpenUser(i) }},
		{"mdbox", func(i *mailbox.UserInfo) mailbox.UserMailbox { return mdbox.New().OpenUser(i) }},
	} {
		t.Run(tc.driver, func(t *testing.T) {
			home := t.TempDir()
			info := &mailbox.UserInfo{Username: "u1@example.com", Home: home, Driver: tc.driver}
			box := tc.open(info)
			if err := box.Init(); err != nil {
				t.Fatal(err)
			}
			idx := indexfile.New().OpenUser(info)
			f, err := idx.OpenFolder("INBOX", 1)
			if err != nil {
				t.Fatal(err)
			}
			body := "From: a@b\r\n\r\nsize me\r\n"
			saved, vsize, guid, err := box.Save("INBOX", strings.NewReader(body), 0, int64(len(body)), nil, nil, [16]byte{})
			if err != nil {
				t.Fatal(err)
			}
			m := &mailbox.MessageMeta{Size: uint32(len(body)), VSize: vsize, GUID: guid}
			if err := mailboxbase.RecordSaved(idx, box, f.ID, "INBOX", saved, m); err != nil {
				t.Fatal(err)
			}
			idx.Close() //nolint:errcheck
			box.Close() //nolint:errcheck

			fresh := indexfile.New().OpenUser(info)
			defer fresh.Close() //nolint:errcheck
			f2, err := fresh.OpenFolder("INBOX", 0)
			if err != nil {
				t.Fatal(err)
			}
			msgs, err := fresh.GetMessages(f2.ID, mailbox.SeqSet{{From: 1, To: 0}})
			if err != nil {
				t.Fatal(err)
			}
			if len(msgs) != 1 {
				t.Fatalf("got %d messages, want 1", len(msgs))
			}
			if got := msgs[0].Size; got != uint32(len(body)) {
				t.Errorf("uid %d reads size %d, want %d: RFC822.SIZE and POP3 LIST answer this",
					msgs[0].UID, got, len(body))
			}
		})
	}
}

// The store we leave has the shape the reference leaves: messages under
// u.<uid>, the index beside them, and nothing else (#1700).
func TestTheFolderWeLeaveHasTheReferenceShape(t *testing.T) {
	home := t.TempDir()
	info := &mailbox.UserInfo{Username: "u1@example.com", Home: home, Driver: "sdbox"}
	box := dboxv2.New().OpenUser(info)
	defer box.Close() //nolint:errcheck
	if err := box.Init(); err != nil {
		t.Fatal(err)
	}
	idx := indexfile.New().OpenUser(info)
	defer idx.Close() //nolint:errcheck
	f, err := idx.OpenFolder("INBOX", 1)
	if err != nil {
		t.Fatal(err)
	}
	body := "From: a@a.com\r\n\r\nsdbox body\r\n"
	for i := 0; i < 2; i++ {
		saved, vsize, guid, serr := box.Save("INBOX", strings.NewReader(body), 0, int64(len(body)), nil, nil, [16]byte{})
		if serr != nil {
			t.Fatal(serr)
		}
		m := &mailbox.MessageMeta{Size: uint32(len(body)), VSize: vsize, GUID: guid}
		if err := mailboxbase.RecordSaved(idx, box, f.ID, "INBOX", saved, m); err != nil {
			t.Fatal(err)
		}
	}

	entries, err := os.ReadDir(filepath.Join(home, "sdbox", "mailboxes", "INBOX", "dbox-Mails"))
	if err != nil {
		t.Fatal(err)
	}
	var messages, others []string
	for _, e := range entries {
		switch {
		case strings.HasPrefix(e.Name(), "u."):
			messages = append(messages, e.Name())
		case strings.HasPrefix(e.Name(), "yarilo.index"):
			// The folder's index, which the reference keeps here too.
		default:
			others = append(others, e.Name())
		}
	}
	sort.Strings(messages)
	if len(messages) != 2 || messages[0] != "u.1" || messages[1] != "u.2" {
		t.Errorf("the folder holds %v, and the reference's fixtures are u.1 and u.2", messages)
	}
	if len(others) != 0 {
		t.Errorf("the folder also holds %v -- names the reference does not write", others)
	}

	// And the bytes of one message have the form its own fixture has.
	ours, err := os.ReadFile(filepath.Join(home, "sdbox", "mailboxes", "INBOX", "dbox-Mails", "u.1"))
	if err != nil {
		t.Fatal(err)
	}
	theirs := dboxref.SdboxShort(t)
	if !bytes.HasPrefix(ours, []byte("2 M1e C")) {
		t.Errorf("our file header is %q, theirs %q", head(ours), head(theirs))
	}
	if len(head(ours)) != len(head(theirs)) {
		t.Errorf("our header line is %d bytes, theirs %d", len(head(ours)), len(head(theirs)))
	}
	if ourKeys, theirKeys := metaKeys(t, ours), metaKeys(t, theirs); ourKeys != theirKeys {
		t.Errorf("our trailer carries %q, theirs %q", ourKeys, theirKeys)
	}
}

func head(b []byte) []byte {
	if at := bytes.IndexByte(b, '\n'); at >= 0 {
		return b[:at+1]
	}
	return b
}

// metaKeys returns the metadata keys in the order written.
func metaKeys(t *testing.T, raw []byte) string {
	t.Helper()
	at := bytes.Index(raw, []byte("\n\x01\x03\n"))
	if at < 0 {
		t.Fatalf("no metadata block in %d bytes", len(raw))
	}
	var out []byte
	for _, line := range bytes.Split(raw[at+4:], []byte("\n")) {
		if len(line) == 0 {
			break
		}
		out = append(out, line[0])
	}
	return string(out)
}

// A body of bare LFs, because a CRLF one cannot tell the two numbers apart:
// both drivers normalise on the way in, so the stored form is longer (#1700).
func TestTheSizeIsWhatWasStoredNotWhatWasHandedOver(t *testing.T) {
	for _, tc := range []struct {
		driver string
		open   func(*mailbox.UserInfo) mailbox.UserMailbox
	}{
		{"sdbox", func(i *mailbox.UserInfo) mailbox.UserMailbox { return dboxv2.New().OpenUser(i) }},
		{"mdbox", func(i *mailbox.UserInfo) mailbox.UserMailbox { return mdbox.New().OpenUser(i) }},
	} {
		t.Run(tc.driver, func(t *testing.T) {
			home := t.TempDir()
			info := &mailbox.UserInfo{Username: "u1@example.com", Home: home, Driver: tc.driver}
			box := tc.open(info)
			if err := box.Init(); err != nil {
				t.Fatal(err)
			}
			idx := indexfile.New().OpenUser(info)
			f, err := idx.OpenFolder("INBOX", 1)
			if err != nil {
				t.Fatal(err)
			}
			// Three bare LFs, so the stored form is three bytes longer.
			body := "From: a@b\n\nline one\n"
			saved, vsize, guid, err := box.Save("INBOX", strings.NewReader(body), 0, int64(len(body)), nil, nil, [16]byte{})
			if err != nil {
				t.Fatal(err)
			}
			if vsize != uint32(len(body)+3) {
				t.Fatalf("Save reports %d for a %d-byte body with three bare LFs", vsize, len(body))
			}
			m := &mailbox.MessageMeta{Size: uint32(len(body)), VSize: vsize, GUID: guid}
			if err := mailboxbase.RecordSaved(idx, box, f.ID, "INBOX", saved, m); err != nil {
				t.Fatal(err)
			}
			idx.Close() //nolint:errcheck
			box.Close() //nolint:errcheck

			fresh := indexfile.New().OpenUser(info)
			defer fresh.Close() //nolint:errcheck
			f2, err := fresh.OpenFolder("INBOX", 0)
			if err != nil {
				t.Fatal(err)
			}
			msgs, err := fresh.GetMessages(f2.ID, mailbox.SeqSet{{From: 1, To: 0}})
			if err != nil {
				t.Fatal(err)
			}
			if len(msgs) != 1 {
				t.Fatalf("got %d messages, want 1", len(msgs))
			}
			got := msgs[0]
			if got.Size != vsize || got.VSize != vsize {
				t.Errorf("size %d, vsize %d, and the stored form is %d bytes", got.Size, got.VSize, vsize)
			}
			if got.Size == uint32(len(body)) {
				t.Errorf("the size is what the caller handed over (%d), not what was stored", len(body))
			}
		})
	}
}
