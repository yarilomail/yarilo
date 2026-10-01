package file

import (
	"bytes"
	"log/slog"
	"strings"
	"testing"

	"github.com/prometheus/client_golang/prometheus/testutil"

	"github.com/yarilomail/yarilo/internal/storage/mailindex"
	"github.com/yarilomail/yarilo/pkg/mailbox"
)

// seedBase leaves INBOX with these flags per message, uids 1..n, all in the
// base and none in the log; it returns the base path.
func seedBase(t *testing.T, dir string, flags [][]string) string {
	t.Helper()
	a := openIdx(dir, testUser)
	f, err := a.OpenFolder("INBOX", 1, "")
	if err != nil {
		t.Fatal(err)
	}
	for i, fl := range flags {
		if err := a.AppendMessage(f.ID, &mailbox.MessageMeta{UID: uint32(i + 1), Size: 10, Flags: fl}); err != nil {
			t.Fatal(err)
		}
	}
	if err := a.OptimizeIndex(f.ID); err != nil {
		t.Fatal(err)
	}
	base := indexPathFor(a.indexDir("INBOX"))
	a.Close() //nolint:errcheck
	return base
}

// rewriteBaseHeader writes the base back with a header edit, bypassing every
// path that would correct it.
func rewriteBaseHeader(t *testing.T, base string, edit func(*mailindex.Header)) {
	t.Helper()
	mf, err := mailindex.Open(base)
	if err != nil {
		t.Fatal(err)
	}
	edit(&mf.Header)
	if _, err := mailindex.Recreate(mf.ToRecreateInput(base)); err != nil {
		t.Fatal(err)
	}
}

// appendWithoutNextUID logs appends for these uids as a writer whose header
// update was lost: the records reach the log, next_uid does not.
func appendWithoutNextUID(t *testing.T, dir string, uids ...uint32) {
	t.Helper()
	a := openIdx(dir, testUser)
	defer a.Close() //nolint:errcheck
	f, err := a.OpenFolder("INBOX", 0, "")
	if err != nil {
		t.Fatal(err)
	}
	a.mu.Lock()
	fs := a.open[f.ID]
	a.mu.Unlock()
	fs.mu.Lock()
	defer fs.mu.Unlock()
	nextUID := encU32Update(28, 0)[:12]
	for _, uid := range uids {
		if err := fs.appendLocked(&mailbox.MessageMeta{UID: uid, Size: 10}); err != nil {
			t.Fatal(err)
		}
		recs, err := fs.appendLogRecords(fs.file.Records[len(fs.file.Records)-1])
		if err != nil {
			t.Fatal(err)
		}
		var kept [][]byte
		for _, r := range recs {
			if !bytes.HasPrefix(r, nextUID) {
				kept = append(kept, r)
			}
		}
		if len(kept) != len(recs)-1 {
			t.Fatalf("expected to drop exactly the next_uid update, dropped %d", len(recs)-len(kept))
		}
		if err := fs.appendMutLog(kept...); err != nil {
			t.Fatal(err)
		}
	}
}

// A header that disagrees with its records is re-derived from them on open,
// one Warn line and one count per field; one that agrees is left silent.
func TestTheHeaderIsReDerivedFromItsRecords(t *testing.T) {
	seen, deleted := []string{`\Seen`}, []string{`\Deleted`}
	cases := []struct {
		name    string
		flags   [][]string
		edit    func(*mailindex.Header)
		logOnly []uint32
		// wantUID is the uid a transaction's append must receive next.
		wantUID   uint32
		wantHdr   mailindex.Header
		corrected []string
	}{
		{
			name:      "next_uid behind the records",
			flags:     [][]string{nil, nil, nil, nil, nil},
			edit:      func(h *mailindex.Header) { h.NextUID = 3 },
			wantUID:   6,
			wantHdr:   mailindex.Header{NextUID: 7, MessagesCount: 6},
			corrected: []string{"next_uid"},
		},
		{
			name:  "counters that drifted",
			flags: [][]string{seen, seen, deleted, nil, nil},
			edit: func(h *mailindex.Header) {
				h.MessagesCount, h.SeenMessagesCount, h.DeletedMessagesCount = 9, 7, 4
			},
			wantUID:   6,
			wantHdr:   mailindex.Header{NextUID: 7, MessagesCount: 6, SeenMessagesCount: 2, DeletedMessagesCount: 1},
			corrected: []string{"messages", "seen", "deleted"},
		},
		{
			name:    "a header that agrees",
			flags:   [][]string{seen, nil, nil},
			edit:    func(*mailindex.Header) {},
			wantUID: 4,
			wantHdr: mailindex.Header{NextUID: 5, MessagesCount: 4, SeenMessagesCount: 1},
		},
		{
			name:      "next_uid behind after the log is replayed",
			flags:     [][]string{nil, nil, nil},
			edit:      func(*mailindex.Header) {},
			logOnly:   []uint32{4, 5},
			wantUID:   6,
			wantHdr:   mailindex.Header{NextUID: 7, MessagesCount: 6},
			corrected: []string{"next_uid"},
		},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			dir := t.TempDir()
			base := seedBase(t, dir, tc.flags)
			rewriteBaseHeader(t, base, tc.edit)
			if len(tc.logOnly) > 0 {
				appendWithoutNextUID(t, dir, tc.logOnly...)
			}

			var logged bytes.Buffer
			prev := slog.Default()
			slog.SetDefault(slog.New(slog.NewTextHandler(&logged, nil)))
			defer slog.SetDefault(prev)
			before := map[string]float64{}
			for _, f := range []string{"next_uid", "messages", "seen", "deleted"} {
				before[f] = testutil.ToFloat64(metricHeaderCorrected.WithLabelValues(f))
			}

			b := openIdx(dir, testUser)
			defer b.Close() //nolint:errcheck
			f, err := b.OpenFolder("INBOX", 0, "")
			if err != nil {
				t.Fatal(err)
			}
			tx, err := b.Begin(f.ID)
			if err != nil {
				t.Fatal(err)
			}
			m := &mailbox.MessageMeta{Size: 10}
			tx.Append(m)
			if _, err := tx.Commit(); err != nil {
				t.Fatal(err)
			}
			if m.UID != tc.wantUID {
				t.Errorf("the append received uid %d, want %d", m.UID, tc.wantUID)
			}

			b.mu.Lock()
			fs := b.open[f.ID]
			b.mu.Unlock()
			fs.mu.Lock()
			h := fs.file.Header
			fs.mu.Unlock()
			if h.NextUID != tc.wantHdr.NextUID || h.MessagesCount != tc.wantHdr.MessagesCount ||
				h.SeenMessagesCount != tc.wantHdr.SeenMessagesCount || h.DeletedMessagesCount != tc.wantHdr.DeletedMessagesCount {
				t.Errorf("header next_uid=%d messages=%d seen=%d deleted=%d, want %d/%d/%d/%d",
					h.NextUID, h.MessagesCount, h.SeenMessagesCount, h.DeletedMessagesCount,
					tc.wantHdr.NextUID, tc.wantHdr.MessagesCount, tc.wantHdr.SeenMessagesCount, tc.wantHdr.DeletedMessagesCount)
			}

			lines := 0
			for _, l := range strings.Split(logged.String(), "\n") {
				if strings.Contains(l, "header corrected from its records") {
					lines++
				}
			}
			if lines != len(tc.corrected) {
				t.Errorf("%d correction lines logged, want %d:\n%s", lines, len(tc.corrected), logged.String())
			}
			for _, field := range tc.corrected {
				if !strings.Contains(logged.String(), "field="+field) {
					t.Errorf("no correction logged for %s", field)
				}
			}
			counted := 0.0
			for f, was := range before {
				counted += testutil.ToFloat64(metricHeaderCorrected.WithLabelValues(f)) - was
			}
			if int(counted) != len(tc.corrected) {
				t.Errorf("fileindex_header_corrected_total rose by %v, want %d", counted, len(tc.corrected))
			}
		})
	}
}
