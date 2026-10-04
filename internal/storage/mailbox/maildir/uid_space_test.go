package maildir

import (
	"errors"
	"fmt"
	"os"
	"sort"
	"strings"
	"testing"

	fileidx "github.com/yarilomail/yarilo/internal/storage/index/file"
	"github.com/yarilomail/yarilo/internal/storage/mailboxbase"
	"github.com/yarilomail/yarilo/pkg/mailbox"
)

// writeList replaces INBOX's list with this header and these rows.
func writeList(t *testing.T, box *userMailbox, header string, rows ...string) {
	t.Helper()
	body := header + "\n"
	for _, r := range rows {
		body += r + "\n"
	}
	if err := os.WriteFile(box.uidListPath("INBOX"), []byte(body), 0o600); err != nil {
		t.Fatal(err)
	}
}

func folderUIDs(t *testing.T, idx mailbox.UserIndex, folder *mailbox.Folder) []uint32 {
	t.Helper()
	msgs, err := idx.GetMessages(folder.ID, mailbox.SeqSet{{From: 1, To: 0}})
	if err != nil {
		t.Fatal(err)
	}
	var out []uint32
	for _, m := range msgs {
		out = append(out, m.UID)
	}
	sort.Slice(out, func(i, j int) bool { return out[i] < out[j] })
	return out
}

// The list and the index name one UID space; which side wins depends only on
// whether the list carries a UIDVALIDITY and whether it is the index's (#2083).
func TestTheListAndTheIndexNameOneUIDSpace(t *testing.T) {
	files := []string{"1700000001.M1P1.host", "1700000002.M2P2.host", "1700000003.M3P3.host"}
	cases := []struct {
		name   string
		header string
		rows   []string
		// wantV zero means the index's own UIDVALIDITY is kept.
		wantV    uint32
		wantUIDs []uint32
		wantNext uint32
		// extra is a file no row names: the pass imports it, writing the list.
		extra string
	}{
		{
			name:     "another generation: the index takes the list's space",
			header:   "3 V777 N14 G00000000000000000000000000000000",
			rows:     []string{"11 :" + files[0] + ":2,", "12 :" + files[1] + ":2,", "13 :" + files[2] + ":2,"},
			wantV:    777,
			wantUIDs: []uint32{11, 12, 13},
			wantNext: 14,
		},
		{
			// Its next uid ahead of the index's: the index raises it under its
			// hold, and the missing UIDVALIDITY must not read as a new one there.
			name:     "a list without one and ahead: next uid raised, uids kept",
			header:   "3 V0 N9 G00000000000000000000000000000000",
			rows:     []string{"1 :" + files[0] + ":2,", "2 :" + files[1] + ":2,", "3 :" + files[2] + ":2,"},
			wantUIDs: []uint32{1, 2, 3},
			wantNext: 9,
		},
		{
			name:     "a list without one, seeded by the pass's own import",
			header:   "3 V0 N4 G00000000000000000000000000000000",
			rows:     []string{"1 :" + files[0] + ":2,", "2 :" + files[1] + ":2,", "3 :" + files[2] + ":2,"},
			extra:    "1700000004.M4P4.host",
			wantUIDs: []uint32{1, 2, 3, 4},
			wantNext: 5,
		},
		{
			name:     "a list without one: it takes the index's, uids kept",
			header:   "3 V0 N4 G00000000000000000000000000000000",
			rows:     []string{"1 :" + files[0] + ":2,", "2 :" + files[1] + ":2,", "3 :" + files[2] + ":2,"},
			wantUIDs: []uint32{1, 2, 3},
		},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			box, idx, folder := recSetup(t)
			for i, f := range files {
				deliverToCur(t, box, f+":2,", "From: a@b\r\n\r\nx\r\n")
				if err := idx.AppendMessage(folder.ID, &mailbox.MessageMeta{UID: uint32(i + 1), Size: 16}); err != nil {
					t.Fatal(err)
				}
			}
			was := folder.UIDValidity
			writeList(t, box, tc.header, tc.rows...)
			if tc.extra != "" {
				deliverToCur(t, box, tc.extra+":2,", "From: a@b\r\n\r\nx\r\n")
			}

			if _, err := box.ReconcileIndex(mailboxbase.Open(box, idx), idx, folder); err != nil {
				t.Fatal(err)
			}
			after, err := idx.OpenFolder("INBOX", 0)
			if err != nil {
				t.Fatal(err)
			}
			wantV := tc.wantV
			if wantV == 0 {
				wantV = was
			}
			if after.UIDValidity != wantV {
				t.Errorf("the folder's UIDVALIDITY is %d, want %d", after.UIDValidity, wantV)
			}
			got := folderUIDs(t, idx, after)
			if fmt.Sprint(got) != fmt.Sprint(tc.wantUIDs) {
				t.Errorf("the folder holds uids %v, want %v", got, tc.wantUIDs)
			}
			if tc.wantNext != 0 && after.NextUID != tc.wantNext {
				t.Errorf("the folder's next uid is %d, want %d", after.NextUID, tc.wantNext)
			}
			v, _, have := box.UIDSpace("INBOX")
			if !have || v != wantV {
				t.Errorf("the list names UIDVALIDITY %d (have %v), want %d", v, have, wantV)
			}
		})
	}
}

// A delivery that reaches a taken-over store before any session opened it gets
// a uid past the list's own, not the fresh index's first (#2083).
func TestADeliveryBeforeTheFirstOpenTakesTheListsNextUID(t *testing.T) {
	root := t.TempDir()
	const user = "u@x.com"
	info := &mailbox.UserInfo{Username: user, Home: testHome(root, user)}
	box := New().OpenUser(info).(*userMailbox)
	if err := box.Init(); err != nil {
		t.Fatal(err)
	}
	if err := box.Create("INBOX"); err != nil {
		t.Fatal(err)
	}
	deliverToCur(t, box, "1700000001.M1P1.host:2,", "From: a@b\r\n\r\nx\r\n")
	writeList(t, box, "3 V900 N50 G00000000000000000000000000000000", "1 :1700000001.M1P1.host:2,")

	idx := fileidx.New().OpenUser(info)
	defer idx.Close() //nolint:errcheck
	mbox := mailboxbase.Open(box, idx, mailboxbase.SaveOnly())
	f, err := mbox.Folder("INBOX", 0)
	if err != nil {
		t.Fatal(err)
	}
	body := "From: c@d\r\n\r\ny\r\n"
	name, _, _, err := box.Save("INBOX", strings.NewReader(body), 0, int64(len(body)), nil, nil, [16]byte{})
	if err != nil {
		t.Fatal(err)
	}
	m := &mailbox.MessageMeta{Size: uint32(len(body))}
	if err := mbox.RecordSaved(f, "INBOX", name, m); err != nil {
		t.Fatalf("record the delivery: %v", err)
	}
	if m.UID != 50 {
		t.Errorf("the delivery got uid %d, want 50: the list already gave away the ones below", m.UID)
	}
}

// A uid the list gives one file is refused to another, on the appended path and
// on the rewrite alike: a second row would put two messages under it (#2083).
func TestAUIDTheListGivesOneFileIsRefusedToAnother(t *testing.T) {
	for _, rewrite := range []bool{false, true} {
		t.Run(fmt.Sprintf("rewrite=%v", rewrite), func(t *testing.T) {
			box, _ := newBox(t, "u@x.com")
			if err := box.Init(); err != nil {
				t.Fatal(err)
			}
			first := saveAndRecord(t, box, "INBOX", "From: a@b\r\n\r\nx\r\n", 5, nil)
			body := "From: c@d\r\n\r\ny\r\n"
			second, _, _, err := box.Save("INBOX", strings.NewReader(body), 0, int64(len(body)), nil, nil, [16]byte{})
			if err != nil {
				t.Fatal(err)
			}
			if rewrite {
				box.folderCacheFor("INBOX").invalidateUIDs("test")
			}
			_, err = box.AssignUID("INBOX", second, 5)
			if !errors.Is(err, mailbox.ErrUIDInUse) {
				t.Fatalf("a second file under uid 5 was not refused: %v", err)
			}
			rows := 0
			for _, rec := range readUIDList(t, box) {
				if strings.HasPrefix(rec, "5 ") {
					rows++
				}
			}
			if rows != 1 {
				t.Errorf("uid 5 has %d rows, want 1 (%s)", rows, first)
			}
		})
	}
}

// The header's next uid can trail the rows; the list is read past it, as the
// reference reads it, from the file and from the cache alike.
func TestTheListsNextUIDIsReadPastTheHeader(t *testing.T) {
	for _, cached := range []bool{false, true} {
		t.Run(fmt.Sprintf("cached=%v", cached), func(t *testing.T) {
			box, _ := newBox(t, "u@x.com")
			if err := box.Init(); err != nil {
				t.Fatal(err)
			}
			writeList(t, box, "3 V9 N2 G00000000000000000000000000000000", "1 :1700000001.M1P1.host:2,", "7 :1700000007.M7P7.host:2,")
			if cached {
				if _, ok := box.UIDFor("INBOX", "1700000001.M1P1.host:2,"); !ok {
					t.Fatal("the list was not loaded into the cache")
				}
			}
			if _, n, _ := box.UIDSpace("INBOX"); n != 8 {
				t.Errorf("the list's next uid reads %d, want 8 past row 7", n)
			}
		})
	}
}

func readUIDList(t *testing.T, box *userMailbox) []string {
	t.Helper()
	data, err := os.ReadFile(box.uidListPath("INBOX"))
	if err != nil {
		t.Fatal(err)
	}
	lines := strings.Split(strings.TrimSpace(string(data)), "\n")
	return lines[1:]
}

// A file given a second uid keeps one row, under the second: the row is the
// file's, replaced by its base, never a second line beside the first.
func TestAFileGivenAnotherUIDKeepsOneRow(t *testing.T) {
	box, _ := newBox(t, "u@x.com")
	if err := box.Init(); err != nil {
		t.Fatal(err)
	}
	body := "From: a@b\r\n\r\nx\r\n"
	name, _, _, err := box.Save("INBOX", strings.NewReader(body), 0, int64(len(body)), nil, nil, [16]byte{})
	if err != nil {
		t.Fatal(err)
	}
	for _, uid := range []uint32{1, 7} {
		if _, err := box.AssignUID("INBOX", name, uid); err != nil {
			t.Fatalf("assign uid %d: %v", uid, err)
		}
	}
	rows := readUIDList(t, box)
	if len(rows) != 1 || !strings.HasPrefix(rows[0], "7 ") {
		t.Errorf("the list holds %v, want the file's one row under uid 7", rows)
	}
}
