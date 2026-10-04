package file

import (
	"errors"
	"os"
	"syscall"
	"testing"

	"github.com/prometheus/client_golang/prometheus/testutil"

	"github.com/yarilomail/yarilo/pkg/mailbox"
)

func journalWriteFailures(t *testing.T, reason string) float64 {
	t.Helper()
	return testutil.ToFloat64(metricJournalWriteFailed.WithLabelValues(reason))
}

// A refused write is classified where it is born: the caller above has only the
// text, and a full volume answered as a fault tells the client to stop trying.
func TestJournalWriteClassifiesTheRefusal(t *testing.T) {
	cases := []struct {
		name    string
		refuse  error
		noSpace bool
		reason  string
	}{
		{name: "a full volume", refuse: syscall.ENOSPC, noSpace: true, reason: "no-space"},
		{name: "an exhausted quota", refuse: syscall.EDQUOT, noSpace: true, reason: "no-space"},
		{name: "a failing disk", refuse: syscall.EIO, noSpace: false, reason: "other"},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			dir := t.TempDir()
			b := openIdx(dir, testUser)
			f, err := b.OpenFolder("INBOX", 1, "")
			if err != nil {
				t.Fatalf("OpenFolder: %v", err)
			}
			wasReason := journalWriteFailures(t, tc.reason)
			wasOther := journalWriteFailures(t, "other")

			mutLogWrite = func(f *os.File, buf []byte) (int, error) {
				n, werr := f.Write(buf[:len(buf)/2])
				if werr != nil {
					return n, werr
				}
				return n, tc.refuse
			}
			t.Cleanup(restoreMutLogSeams)

			tx, err := b.Begin(f.ID)
			if err != nil {
				t.Fatalf("Begin: %v", err)
			}
			tx.Append(&mailbox.MessageMeta{Size: 20})
			_, err = tx.Commit()
			if err == nil {
				t.Fatal("Commit succeeded while the log refused the write")
			}
			if !errors.Is(err, tc.refuse) {
				t.Errorf("Commit err = %v, want the volume's own error", err)
			}

			var nospace *mailbox.NoSpaceError
			if got := errors.As(err, &nospace); got != tc.noSpace {
				t.Errorf("errors.As NoSpaceError = %v, want %v for %v", got, tc.noSpace, tc.refuse)
			}
			if tc.noSpace && nospace != nil {
				if !errors.Is(err, mailbox.ErrNoSpace) {
					t.Error("the error does not carry the resource class, so no caller can answer 'later'")
				}
				if nospace.Folder != "INBOX" {
					t.Errorf("the refusal names folder %q, want INBOX", nospace.Folder)
				}
			} else if errors.Is(err, mailbox.ErrNoSpace) {
				t.Error("a failing disk was classified as a resource condition")
			}

			if got := journalWriteFailures(t, tc.reason); got != wasReason+1 {
				t.Errorf("%s counter = %v, want %v", tc.reason, got, wasReason+1)
			}
			if tc.noSpace {
				if got := journalWriteFailures(t, "other"); got != wasOther {
					t.Errorf("other counter moved to %v: a full volume was counted as a fault", got)
				}
			}
		})
	}
}

// A full volume leaves nothing damaged: the records are what they were, so the
// folder must not carry the marker that sends the next open into a rebuild.
func TestNoSpaceLeavesTheFolderUnmarked(t *testing.T) {
	dir := t.TempDir()
	b := openIdx(dir, testUser)
	f, err := b.OpenFolder("INBOX", 1, "")
	if err != nil {
		t.Fatalf("OpenFolder: %v", err)
	}
	mutLogWrite = func(f *os.File, buf []byte) (int, error) {
		n, werr := f.Write(buf[:len(buf)/2])
		if werr != nil {
			return n, werr
		}
		return n, syscall.ENOSPC
	}
	t.Cleanup(restoreMutLogSeams)

	tx, err := b.Begin(f.ID)
	if err != nil {
		t.Fatalf("Begin: %v", err)
	}
	tx.Append(&mailbox.MessageMeta{Size: 20})
	if _, err := tx.Commit(); err == nil {
		t.Fatal("Commit succeeded while the volume was full")
	}
	restoreMutLogSeams()

	reader := openIdx(dir, testUser)
	rf, err := reader.OpenFolder("INBOX", 1, "")
	if err != nil {
		t.Fatalf("reopen: %v", err)
	}
	if rf.Fsckd {
		t.Error("the folder carries the corruption marker after a full volume: the next open rebuilds an intact index")
	}
}
