package file

import (
	"io"
	"os"
	"slices"
	"syscall"
	"testing"

	"github.com/yarilomail/yarilo/v2/internal/storage/mailindex"
	"github.com/yarilomail/yarilo/v2/pkg/mailbox"
)

type movedReader struct {
	io.ReaderAt
	err   error
	short bool
}

func (m movedReader) ReadAt(p []byte, off int64) (int, error) {
	switch {
	case m.short:
		n, _ := m.ReaderAt.ReadAt(p[:len(p)/2], off)
		return n, io.EOF
	case m.err != nil:
		return 0, m.err
	}
	return m.ReaderAt.ReadAt(p, off)
}

// movedFolder is a folder of n whose vsize is current, so a view is built on
// it without reading a record.
func movedFolder(t *testing.T, n int) string {
	t.Helper()
	home := acceptFolder(t, n)
	h, f := acceptOpen(t, home)
	if err := h.(*userHandle).RecomputeVSize(f.ID); err != nil {
		t.Fatal(err)
	}
	h.Close() //nolint:errcheck
	return home
}

// A chunk read from a base another pod replaced is a moved base: the
// session's next read answers from the current base, with that pod's change
// (INDEX-MEMORY.md §4).
func TestAReadOfAReplacedBaseAnswersFromTheCurrentOne(t *testing.T) {
	cases := []struct {
		name string
		// in runs where the chunk read starts, on the base the session opened.
		in func(t *testing.T, path string, r io.ReaderAt, fold func()) io.ReaderAt
	}{
		{name: "stale handle", in: func(_ *testing.T, _ string, r io.ReaderAt, fold func()) io.ReaderAt {
			fold()
			return movedReader{ReaderAt: r, err: syscall.ESTALE}
		}},
		{name: "short read", in: func(_ *testing.T, _ string, r io.ReaderAt, fold func()) io.ReaderAt {
			fold()
			return movedReader{ReaderAt: r, short: true}
		}},
		{name: "the lineage changed in place", in: func(t *testing.T, path string, r io.ReaderAt, fold func()) io.ReaderAt {
			if err := os.Link(path, path+".old"); err != nil {
				t.Fatal(err)
			}
			fold()
			raw, err := os.ReadFile(path)
			if err != nil {
				t.Fatal(err)
			}
			if err := os.WriteFile(path+".old", raw, 0o600); err != nil {
				t.Fatal(err)
			}
			if err := os.Rename(path+".old", path); err != nil {
				t.Fatal(err)
			}
			return r
		}},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			const n = 3 * mailindex.ChunkRecords
			home := movedFolder(t, n)
			reader, f := acceptOpen(t, home)
			defer reader.Close() //nolint:errcheck
			opens := mailindex.LazyOpens()
			if _, _, err := reader.(*userHandle).FolderVSize(f.ID); err != nil {
				t.Fatal(err)
			}
			if got := mailindex.LazyOpens() - opens; got != 0 {
				t.Fatalf("a vsize read loaded records %d times", got)
			}

			other, of := acceptOpen(t, home)
			defer other.Close() //nolint:errcheck
			fold := func() {
				if err := other.UpdateFlags(of.ID, 5, []string{`\Flagged`}, nil); err != nil {
					t.Fatal(err)
				}
				if err := other.OptimizeIndex(of.ID); err != nil {
					t.Fatal(err)
				}
			}
			fired := false
			mailindex.ChunkReadSeam = func(path string, r io.ReaderAt) io.ReaderAt {
				if fired {
					return r
				}
				fired = true
				return c.in(t, path, r, fold)
			}
			defer func() { mailindex.ChunkReadSeam = nil }()

			msgs, err := reader.GetMessages(f.ID, mailbox.SeqSet{})
			if err != nil {
				t.Fatalf("the read over a replaced base failed: %v", err)
			}
			if !fired {
				t.Fatal("the read loaded no chunk")
			}
			if len(msgs) != n {
				t.Fatalf("%d messages, want %d", len(msgs), n)
			}
			if !slices.Contains(msgs[4].Flags, `\Flagged`) {
				t.Errorf("uid 5 has %v, want the other session's \\Flagged", msgs[4].Flags)
			}
		})
	}
}

// What a session's commands cost in opens of the base, on a folder of 10000:
// one for the first read of records, none after it.
func TestASessionOpensTheBaseOncePerView(t *testing.T) {
	home := movedFolder(t, 10000)
	h, f := acceptOpen(t, home)
	defer h.Close() //nolint:errcheck
	opens := mailindex.LazyOpens()
	steps := []struct {
		name  string
		read  func() error
		total int64
	}{
		{"vsize", func() error { _, _, err := h.(*userHandle).FolderVSize(f.ID); return err }, 0},
		{"fetch all", func() error { _, err := h.GetMessages(f.ID, mailbox.SeqSet{}); return err }, 1},
		{"fetch scattered uids", func() error {
			_, err := h.GetMessages(f.ID, mailbox.SeqSet{{From: 17, To: 17}, {From: 4100, To: 4100}, {From: 9999, To: 9999}})
			return err
		}, 1},
		{"fetch all again", func() error { _, err := h.GetMessages(f.ID, mailbox.SeqSet{}); return err }, 1},
	}
	for _, s := range steps {
		if err := s.read(); err != nil {
			t.Fatal(err)
		}
		if got := mailindex.LazyOpens() - opens; got != s.total {
			t.Errorf("after %s the base was opened %d times, want %d", s.name, got, s.total)
		}
	}
}

// A handle that left its records on disk after a lazy read writes on the
// whole folder: its own write, and another's still in the journal, are kept.
func TestAWriteAfterALazyReadKeepsEveryRecord(t *testing.T) {
	for _, other := range []bool{false, true} {
		t.Run(map[bool]string{false: "nothing changed since", true: "another wrote since"}[other], func(t *testing.T) {
			const n = 2 * mailindex.ChunkRecords
			home := movedFolder(t, n)
			h, f := acceptOpen(t, home)
			defer h.Close() //nolint:errcheck
			if _, err := h.GetMessages(f.ID, mailbox.SeqSet{}); err != nil {
				t.Fatal(err)
			}
			if other {
				o, of := acceptOpen(t, home)
				if err := o.UpdateFlags(of.ID, 9, []string{`\Answered`}, nil); err != nil {
					t.Fatal(err)
				}
				o.Close() //nolint:errcheck
			}
			if err := h.UpdateFlags(f.ID, 7, []string{`\Flagged`}, nil); err != nil {
				t.Fatal(err)
			}
			if err := h.OptimizeIndex(f.ID); err != nil {
				t.Fatal(err)
			}
			fresh, ff := acceptOpen(t, home)
			defer fresh.Close() //nolint:errcheck
			msgs, err := fresh.GetMessages(ff.ID, mailbox.SeqSet{})
			if err != nil {
				t.Fatal(err)
			}
			if len(msgs) != n || ff.Messages != n {
				t.Fatalf("%d messages, header %d, want %d", len(msgs), ff.Messages, n)
			}
			if !slices.Contains(msgs[6].Flags, `\Flagged`) || other && !slices.Contains(msgs[8].Flags, `\Answered`) {
				t.Errorf("uid 7 %v, uid 9 %v: a write was lost", msgs[6].Flags, msgs[8].Flags)
			}
		})
	}
}
