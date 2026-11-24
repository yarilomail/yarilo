package file

import (
	"fmt"
	"os"
	"os/exec"
	"strconv"
	"strings"
	"testing"
	"time"

	"github.com/prometheus/client_golang/prometheus/testutil"

	"github.com/yarilomail/yarilo/v2/internal/storage/mailindex"
	"github.com/yarilomail/yarilo/v2/pkg/mailbox"
)

// The other process for the reload rows: in one process the folder state is
// shared between handles, so a fold inside a reload comes only from another.
// Each round of YARILO_TEST_RELOAD_ROUNDS ("seen:expunge,...") marks one
// message seen, expunges one and folds; a round ending in "!" does not fold.
func TestReloadFoldsHelper(t *testing.T) {
	home := os.Getenv("YARILO_TEST_RELOAD_HOME")
	if home == "" {
		t.Skip("helper: run by TestAReloadAcrossTwoFoldsReadsAgain")
	}
	ui := New().OpenUser(&mailbox.UserInfo{Username: testUser, Home: home}).(*userHandle).ui
	defer ui.Close() //nolint:errcheck
	f, err := ui.OpenFolder("INBOX", 0, "")
	if err != nil {
		t.Fatal(err)
	}
	for _, round := range strings.Split(os.Getenv("YARILO_TEST_RELOAD_ROUNDS"), ",") {
		round, keep := strings.CutSuffix(round, "!")
		seen, gone, _ := strings.Cut(round, ":")
		s, _ := strconv.Atoi(seen)
		g, _ := strconv.Atoi(gone)
		if err := ui.AddFlags(f.ID, uint32(s), []string{`\Seen`}, nil); err != nil {
			t.Fatal(err)
		}
		if err := ui.ExpungeMessage(f.ID, uint32(g)); err != nil {
			t.Fatal(err)
		}
		if keep {
			continue
		}
		if err := ui.OptimizeIndex(f.ID); err != nil {
			t.Fatal(err)
		}
	}
}

func foldFromAnotherProcess(t *testing.T, home, rounds string) {
	t.Helper()
	cmd := exec.CommandContext(t.Context(), os.Args[0], "-test.run=^TestReloadFoldsHelper$", "-test.v")
	cmd.Env = append(os.Environ(), "YARILO_TEST_RELOAD_HOME="+home, "YARILO_TEST_RELOAD_ROUNDS="+rounds)
	cmd.WaitDelay = time.Second
	if out, err := cmd.CombinedOutput(); err != nil {
		t.Errorf("the folding process failed: %v\n%s", err, out)
	}
}

// A fold the reload sees by the base's stat sends it to read the base again,
// and changes after it go to the new log; two more folds between that log's
// open and the base's read leave a base the log does not pair with. The reload reads
// both again; replaying that log whole put its old absolute counters on the
// new base (2.4.8 windows: messages 200 on 199 records, seen 0 on 17).
func TestAReloadAcrossTwoFoldsReadsAgain(t *testing.T) {
	home := t.TempDir()
	open := func() *userIndex {
		return New().OpenUser(&mailbox.UserInfo{Username: testUser, Home: home}).(*userHandle).ui
	}
	r := open()
	t.Cleanup(func() { _ = r.Close() })
	f, err := r.OpenFolder("INBOX", 42, "")
	if err != nil {
		t.Fatal(err)
	}
	for i := 1; i <= 20; i++ {
		if err := r.AppendMessage(f.ID, &mailbox.MessageMeta{UID: uint32(i), Size: 10}); err != nil {
			t.Fatal(err)
		}
	}
	fs, err := r.state(f.ID)
	if err != nil {
		t.Fatal(err)
	}
	foldFromAnotherProcess(t, home, "3:5,9:10!")
	folded := false
	afterReloadLogOpen = func() {
		if !folded {
			folded = true
			foldFromAnotherProcess(t, home, "4:6,7:8")
		}
	}
	t.Cleanup(func() { afterReloadLogOpen = nil })
	out := logged(t, func() {
		fs.mu.Lock()
		defer fs.mu.Unlock()
		if err := fs.reloadNow(); err != nil {
			t.Errorf("reload: %v", err)
		}
		if err := fs.recountHeaderLocked(); err != nil {
			t.Error(err)
		}
	})
	if !folded {
		t.Fatal("the folds never ran inside the reload, so this row proves nothing")
	}
	fs.mu.Lock()
	h, n := fs.file.Header, fs.recordCount()
	fs.mu.Unlock()
	if n != 16 || h.MessagesCount != 16 || h.SeenMessagesCount != 4 {
		t.Errorf("reader after three folds: %d records, header messages=%d seen=%d, want 16/16/4", n, h.MessagesCount, h.SeenMessagesCount)
	}
	if strings.Contains(out, "header corrected") {
		t.Errorf("the reload paired a log with a base it does not meet:\n%s", out)
	}
}

// Which base and log pairs replay the log whole, and which were read at two
// moments: only the second are read again.
func TestOnlyAPairReadAtTwoMomentsIsStale(t *testing.T) {
	logOf := func(seq uint32) *logReader {
		if seq == lineageUnknown {
			return &logReader{} // no readable header
		}
		return &logReader{ok: true, hdr: mailindex.LogHeader{FileSeq: seq}}
	}
	for _, tc := range []struct {
		name  string
		base  lineageHdr
		log   uint32
		stale bool
	}{
		{"the base's own log", lineageHdr{Lineage: 7, FoldedLineage: 6, FoldedOffset: 100}, 7, false},
		{"the log the base folded", lineageHdr{Lineage: 7, FoldedLineage: 6, FoldedOffset: 100}, 6, false},
		{"a base without a lineage replays whole", lineageHdr{}, 6, false},
		{"a log without a header has nothing to replay", lineageHdr{Lineage: 7, FoldedLineage: 6}, lineageUnknown, false},
		{"a pre-extension log under the first fold after an upgrade", lineageHdr{Lineage: 2}, legacyLogLineage, false},
		{"a log two folds behind the base", lineageHdr{Lineage: 7, FoldedLineage: 6, FoldedOffset: 100}, 5, true},
		{"a log of a later base", lineageHdr{Lineage: 7, FoldedLineage: 6, FoldedOffset: 100}, 8, true},
		{"a pre-extension log under a base that folded a minted one", lineageHdr{Lineage: 7, FoldedLineage: 6}, legacyLogLineage, true},
	} {
		t.Run(tc.name, func(t *testing.T) {
			if got := staleWith(tc.base, logOf(tc.log)); got != tc.stale {
				t.Errorf("staleWith = %v, want %v", got, tc.stale)
			}
		})
	}
}

// Folds inside every attempt: the reload reads again up to its bound, then
// fails, naming the account, and never answers with the log replayed whole.
func TestAReloadThatNeverPairsFailsNamingTheAccount(t *testing.T) {
	home := t.TempDir()
	r := New().OpenUser(&mailbox.UserInfo{Username: testUser, Home: home}).(*userHandle).ui
	t.Cleanup(func() { _ = r.Close() })
	f, err := r.OpenFolder("INBOX", 42, "")
	if err != nil {
		t.Fatal(err)
	}
	for i := 1; i <= 30; i++ {
		if err := r.AppendMessage(f.ID, &mailbox.MessageMeta{UID: uint32(i), Size: 10}); err != nil {
			t.Fatal(err)
		}
	}
	fs, err := r.state(f.ID)
	if err != nil {
		t.Fatal(err)
	}
	foldFromAnotherProcess(t, home, "1:2,3:4!")
	attempts := 0
	afterReloadLogOpen = func() {
		attempts++
		a := uint32(4 * attempts)
		foldFromAnotherProcess(t, home, fmt.Sprintf("%d:%d,%d:%d", a+1, a+2, a+3, a+4))
		// The next attempt's log carries changes, as the stand's did.
		foldFromAnotherProcess(t, home, fmt.Sprintf("%d:%d!", a+25, a+26))
	}
	t.Cleanup(func() { afterReloadLogOpen = nil })
	retried := testutil.ToFloat64(metricStalePair.WithLabelValues("reload", "retried"))
	failed := testutil.ToFloat64(metricStalePair.WithLabelValues("reload", "failed"))
	var rerr error
	out := logged(t, func() {
		fs.mu.Lock()
		defer fs.mu.Unlock()
		rerr = fs.reloadNow()
	})
	if rerr == nil || !strings.Contains(rerr.Error(), testUser) {
		t.Errorf("reload = %v, want an error naming %s", rerr, testUser)
	}
	if attempts != baseReadAttempts+1 {
		t.Errorf("%d attempts, want %d", attempts, baseReadAttempts+1)
	}
	if !strings.Contains(out, "kept not pairing") || !strings.Contains(out, "user="+testUser) {
		t.Errorf("no error line naming the account:\n%s", out)
	}
	if strings.Contains(out, "header corrected") {
		t.Errorf("a stale pair was replayed:\n%s", out)
	}
	if got := testutil.ToFloat64(metricStalePair.WithLabelValues("reload", "retried")) - retried; got != baseReadAttempts {
		t.Errorf("retried rose by %v, want %d", got, baseReadAttempts)
	}
	if got := testutil.ToFloat64(metricStalePair.WithLabelValues("reload", "failed")) - failed; got != 1 {
		t.Errorf("failed rose by %v, want 1", got)
	}
}
