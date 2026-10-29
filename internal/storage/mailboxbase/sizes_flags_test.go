package mailboxbase_test

import (
	"slices"
	"strings"
	"testing"

	"github.com/yarilomail/yarilo/v2/internal/storage/index/file"
	"github.com/yarilomail/yarilo/v2/internal/storage/mailbox/dboxv2"
	"github.com/yarilomail/yarilo/v2/internal/storage/mailboxbase"
	"github.com/yarilomail/yarilo/v2/pkg/mailbox"
)

// The virtual size counts a bare LF as the CRLF the wire carries; a CRLF is
// counted once.
func TestCountSizesCountsBareLFAsCRLF(t *testing.T) {
	for in, want := range map[string][2]uint32{
		"a\r\nb\r\n": {6, 6},
		"a\nb\n":     {4, 6},
		"a\r\nb\n\n": {6, 8},
		"":           {0, 0},
	} {
		size, vsize, err := mailboxbase.CountSizes(strings.NewReader(in))
		if err != nil || size != want[0] || vsize != want[1] {
			t.Errorf("%q: size %d vsize %d err %v, want %v", in, size, vsize, err, want)
		}
	}
}

func sdboxBox(t *testing.T) (*mailboxbase.Box, mailbox.UserIndex, *mailbox.Folder) {
	t.Helper()
	info := &mailbox.UserInfo{Username: "u@example.com", Home: t.TempDir(), Driver: "sdbox"}
	store := dboxv2.New().OpenUser(info)
	if err := store.Init(); err != nil {
		t.Fatal(err)
	}
	idx := file.New().OpenUser(info)
	t.Cleanup(func() { _ = store.Close(); _ = idx.Close() })
	box := mailboxbase.Open(store, idx)
	f, err := box.Folder("INBOX", 0)
	if err != nil {
		t.Fatal(err)
	}
	return box, idx, f
}

// Where names carry no flags the delta goes to the index as asked: added,
// removed, or the set replaced.
func TestAFlagDeltaReachesTheIndexAsAsked(t *testing.T) {
	box, idx, f := sdboxBox(t)
	if err := idx.AppendMessage(f.ID, &mailbox.MessageMeta{UID: 1, Size: 3, Flags: []string{`\Seen`}}); err != nil {
		t.Fatal(err)
	}
	for _, step := range []struct {
		upd  mailbox.FlagsUpdate
		want []string
	}{
		{mailbox.FlagsUpdate{Mode: mailbox.FlagsAdd, Flags: []string{`\Flagged`}}, []string{`\Flagged`, `\Seen`}},
		{mailbox.FlagsUpdate{Mode: mailbox.FlagsRemove, Flags: []string{`\Seen`}}, []string{`\Flagged`}},
		{mailbox.FlagsUpdate{Flags: []string{`\Answered`}}, []string{`\Answered`}},
	} {
		if err := box.UpdateFlags(f.ID, 1, step.upd); err != nil {
			t.Fatal(err)
		}
		msgs, err := idx.GetMessages(f.ID, mailbox.SeqSet{{From: 1, To: 1}})
		if err != nil || len(msgs) != 1 {
			t.Fatalf("%v, %v", msgs, err)
		}
		got := slices.Clone(msgs[0].Flags)
		slices.Sort(got)
		if !slices.Equal(got, step.want) {
			t.Errorf("after %+v: flags %v, want %v", step.upd, got, step.want)
		}
	}
}

// A record that arrived with no size takes the one its message has on disk, so
// the folder's sum stops reading it as empty (#1728).
func TestASizelessRecordTakesItsMessagesSize(t *testing.T) {
	box, idx, f := sdboxBox(t)
	body := "Subject: s\n\nbare LF body\n"
	name, _, guid, err := box.Store().Save("INBOX", strings.NewReader(body), 0, int64(len(body)), nil, nil, [16]byte{})
	if err != nil {
		t.Fatal(err)
	}
	m := &mailbox.MessageMeta{GUID: guid}
	if err := box.RecordSaved(f, "INBOX", name, m); err != nil {
		t.Fatal(err)
	}
	// What an older writer left: the record with its sizes zeroed.
	if _, err := idx.ResetFolder(f.ID, []*mailbox.MessageMeta{{UID: m.UID, GUID: guid}}); err != nil {
		t.Fatal(err)
	}
	n, err := mailboxbase.FillSizelessRecords(idx, box.Store(), f)
	if err != nil {
		t.Fatal(err)
	}
	if n != 1 {
		t.Errorf("stamped %d records, want 1", n)
	}
	msgs, _ := idx.GetMessages(f.ID, mailbox.SeqSet{{From: 1, To: 0}})
	if len(msgs) != 1 || msgs[0].VSize == 0 {
		t.Errorf("records after the stamp: %+v", msgs)
	}
}
